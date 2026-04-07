// otelgotest is a drop-in go test replacement that emits OTel spans for
// each test. Usage:
//
//	otelgotest ./...
//	otelgotest -v -run TestFoo ./mypackage
//
// All flags and arguments are forwarded to go test. The command runs
// go test -json internally, parses the JSON event stream, and emits
// OTel spans via gotest.Run. Human-readable output is reconstructed
// on stdout.
//
// Unlike the old -exec approach (go test -exec otelgotest), this
// preserves test caching.
//
// Cross-process span context propagation is supported via a Unix
// socket (OTEL_TEST_SOCKET). Test binaries using oteltestctx can
// connect to this socket to adopt the externally created span context,
// making in-process operations children of the test span.
//
// If no OTLP endpoint is configured, go test is executed directly
// with no overhead.
package main

import (
	"bufio"
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
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]

	// Detect old -exec usage: go test -exec otelgotest passes
	// a compiled .test binary as the first argument.
	if len(args) > 0 && looksLikeExecMode(args[0]) {
		fmt.Fprintf(os.Stderr, "otelgotest: it looks like you're using the old -exec mode.\n")
		fmt.Fprintf(os.Stderr, "  otelgotest is now a go test wrapper. Use:\n")
		fmt.Fprintf(os.Stderr, "    otelgotest ./...\n")
		fmt.Fprintf(os.Stderr, "  instead of:\n")
		fmt.Fprintf(os.Stderr, "    go test -exec otelgotest ./...\n")
		return 1
	}

	// If no OTLP endpoint, just run go test directly.
	if !hasOTLPEndpoint() {
		return execGoTest(args)
	}

	ctx := context.Background()
	ctx = otel.InitEmbedded(ctx, nil)
	defer otel.Close()

	tp := otelgo.GetTracerProvider()

	// Set up a Unix socket for cross-process span context propagation.
	// Test binaries using oteltestctx can connect to this socket to
	// retrieve the span context of the externally created test span.
	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	tmpDir, err := os.MkdirTemp("", "otelgotest")
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: tmpdir: %v\n", err)
		return execGoTest(args)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "span.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: socket listen: %v\n", err)
		return execGoTest(args)
	}
	defer listener.Close()

	go serveSpanContexts(listener, registry)

	// Build the go test -json command, forwarding all user args.
	goTestArgs := []string{"test", "-json"}
	goTestArgs = append(goTestArgs, stripJSONFlag(args)...)

	cmd := exec.Command("go", goTestArgs...)
	cmd.Env = append(os.Environ(), "OTEL_TEST_SOCKET="+socketPath)
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	jsonOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: stdout pipe: %v\n", err)
		return execGoTest(args)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "otelgotest: start: %v\n", err)
		return execGoTest(args)
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

	cmd.Wait()

	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return 1
}

// execGoTest runs go test with no instrumentation.
func execGoTest(args []string) int {
	goArgs := []string{"test"}
	goArgs = append(goArgs, args...)
	cmd := exec.Command("go", goArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Run()
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return 1
}

// stripJSONFlag removes -json from args since we add it ourselves.
func stripJSONFlag(args []string) []string {
	var out []string
	for _, arg := range args {
		if arg != "-json" {
			out = append(out, arg)
		}
	}
	return out
}

// looksLikeExecMode returns true if arg looks like a compiled test binary,
// indicating the user is trying the old go test -exec usage.
func looksLikeExecMode(arg string) bool {
	return strings.HasSuffix(arg, ".test") || strings.Contains(arg, ".test ")
}

// serveSpanContexts accepts connections on the Unix socket and responds
// with the traceparent of the requested test's span. Each connection is a
// single request/response: the client sends a package-qualified test name
// (e.g., "example.com/pkg/TestFoo") on one line, and the server responds
// with the W3C traceparent on one line.
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
	testKey := scanner.Text()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sc, ok := registry.WaitFor(ctx, testKey)
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
