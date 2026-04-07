// otelgotest is a go test -exec wrapper that emits OTel spans for each
// test. Usage:
//
//	go test -exec otelgotest ./...
//
// When called by go test -exec, the first argument is the compiled test
// binary followed by -test.* flags. The wrapper runs the binary with
// -test.v=test2json, pipes stdout through go tool test2json, and feeds
// the JSON stream to gotest.Run for OTel export.
//
// The original human-readable output is reconstructed and printed to
// stdout so go test can process it normally (e.g. with -json or -v).
//
// If no OTLP endpoint is configured, the test binary is executed
// directly with no overhead.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/gotest"
	otelgo "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: otelgotest <test-binary> [flags...]\n")
		fmt.Fprintf(os.Stderr, "  meant to be used with: go test -exec otelgotest\n")
		os.Exit(1)
	}

	os.Exit(run())
}

func run() int {
	binary := os.Args[1]
	args := os.Args[2:]

	// If no OTLP endpoint, just exec the binary directly.
	if !hasOTLPEndpoint() {
		return execDirect(binary, args)
	}

	ctx := context.Background()
	ctx = otel.InitEmbedded(ctx, nil)
	defer otel.Close()

	tp := otelgo.GetTracerProvider()

	// Replace -test.v with -test.v=test2json for structured output.
	testArgs := forceTest2JSON(args)

	// Detect package name from the binary path (e.g. "foo.test" -> "foo").
	pkg := detectPackage(binary)

	// Set up a Unix socket for cross-process span context propagation.
	// Test binaries using oteltest can connect to this socket to retrieve
	// the span context of the externally created test span, so that
	// in-process operations become children of it.
	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	tmpDir, err := os.MkdirTemp("", "otelgotest")
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: tmpdir: %v\n", err)
		return execDirect(binary, args)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "span.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: socket listen: %v\n", err)
		return execDirect(binary, args)
	}
	defer listener.Close()

	go serveSpanContexts(listener, registry)

	// Run the test binary, capturing stdout.
	testCmd := exec.Command(binary, testArgs...)
	testCmd.Env = append(os.Environ(), "OTEL_TEST_SOCKET="+socketPath)
	testCmd.Stderr = os.Stderr
	testCmd.Stdin = os.Stdin

	testOut, err := testCmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: stdout pipe: %v\n", err)
		return execDirect(binary, args)
	}

	// Pipe through go tool test2json to get JSON events.
	test2jsonArgs := []string{"tool", "test2json"}
	if pkg != "" {
		test2jsonArgs = append(test2jsonArgs, "-p", pkg)
	}
	test2json := exec.Command("go", test2jsonArgs...)
	test2json.Stdin = testOut
	test2json.Stderr = os.Stderr

	jsonOut, err := test2json.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: test2json pipe: %v\n", err)
		return execDirect(binary, args)
	}

	if err := test2json.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: test2json start: %v\n", err)
		return execDirect(binary, args)
	}

	if err := testCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: test binary start: %v\n", err)
		test2json.Process.Kill()
		return execDirect(binary, args)
	}

	// Process JSON events, writing human-readable output to stdout.
	opts := []gotest.Option{
		gotest.WithOutput(os.Stdout),
		gotest.WithSpanContextRegistry(registry),
	}
	if lp := otel.LoggerProvider(ctx); lp != nil {
		opts = append(opts, gotest.WithLoggerProvider(lp))
	}
	gotest.Run(ctx, jsonOut, tp, opts...)

	// Wait for both processes.
	testCmd.Wait()
	test2json.Wait()

	if testCmd.ProcessState != nil {
		return testCmd.ProcessState.ExitCode()
	}
	return 1
}

// execDirect runs the test binary with no instrumentation.
func execDirect(binary string, args []string) int {
	cmd := exec.Command(binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Run()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return 1
}

// forceTest2JSON replaces any -test.v flag with -test.v=test2json
// and ensures the flag is present.
func forceTest2JSON(args []string) []string {
	out := []string{"-test.v=test2json"}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-test.v") {
			out = append(out, arg)
		}
	}
	return out
}

// detectPackage determines the full import path of the package under test.
// When invoked via go test -exec, the working directory is the package's
// source directory, so we walk up to find go.mod and combine the module
// path with the relative directory.
func detectPackage(binary string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return fallbackPackage(binary)
	}

	// Walk up from cwd to find go.mod.
	dir := cwd
	for {
		modPath, err := parseModulePath(filepath.Join(dir, "go.mod"))
		if err == nil {
			rel, err := filepath.Rel(dir, cwd)
			if err != nil || rel == "." {
				return modPath
			}
			return modPath + "/" + filepath.ToSlash(rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return fallbackPackage(binary)
}

// fallbackPackage extracts a short package name from the test binary path.
func fallbackPackage(binary string) string {
	base := filepath.Base(binary)
	return strings.TrimSuffix(base, ".test")
}

// parseModulePath reads the module path from a go.mod file.
func parseModulePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("no module directive found in %s", path)
}

// serveSpanContexts accepts connections on the Unix socket and responds
// with the traceparent of the requested test's span. Each connection is a
// single request/response: the client sends a test name (one line), and
// the server responds with the W3C traceparent (one line).
func serveSpanContexts(listener net.Listener, registry *gotest.SpanContextRegistry) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return // listener closed
		}
		go handleSpanContextConn(conn, registry)
	}
}

func handleSpanContextConn(conn net.Conn, registry *gotest.SpanContextRegistry) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	testName := scanner.Text()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sc, ok := registry.WaitFor(ctx, testName)
	if !ok {
		return
	}

	fmt.Fprintf(conn, "%s\n", formatTraceparent(sc))
}

func formatTraceparent(sc trace.SpanContext) string {
	return fmt.Sprintf("00-%s-%s-%s", sc.TraceID(), sc.SpanID(), sc.TraceFlags())
}

func hasOTLPEndpoint() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}
