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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/gotest"
	otelgo "go.opentelemetry.io/otel"
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

	// Run the test binary, capturing stdout.
	testCmd := exec.Command(binary, testArgs...)
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
	opts := []gotest.Option{gotest.WithOutput(os.Stdout)}
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

// detectPackage extracts a package name from the test binary path.
func detectPackage(binary string) string {
	base := filepath.Base(binary)
	return strings.TrimSuffix(base, ".test")
}

func hasOTLPEndpoint() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}
