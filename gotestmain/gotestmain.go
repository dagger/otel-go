// Package gotestmain provides automatic OTel test reporting via a
// TestMain integration.
//
// Usage:
//
//	func TestMain(m *testing.M) {
//	    os.Exit(gotestmain.Main(m))
//	}
//
// Main captures test output, converts it to OTel spans, and exports
// them via the standard OTEL_* environment variables. If no OTLP
// endpoint is configured, tests run normally with no overhead.
package gotestmain

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/gotest"
	otelgo "go.opentelemetry.io/otel"
)

// Main runs the test suite and emits OTel spans for each test.
// It returns the exit code from m.Run().
//
// Call it from TestMain:
//
//	func TestMain(m *testing.M) {
//	    os.Exit(gotestmain.Main(m))
//	}
func Main(m *testing.M) int {
	// If no OTLP endpoint is configured, skip the whole pipeline.
	if !hasOTLPEndpoint() {
		return m.Run()
	}

	ctx := context.Background()
	ctx = otel.InitEmbedded(ctx, nil)
	defer otel.Close()

	tp := otelgo.GetTracerProvider()

	// Redirect stdout to a pipe so we can capture test2json output.
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return m.Run()
	}
	origStdout := os.Stdout
	os.Stdout = pipeW

	// Force -test.v=test2json for structured output.
	forceTest2JSON()

	// Detect the package name from the binary name.
	pkg := detectPackage()

	// Spawn go tool test2json to convert raw output to JSON.
	test2jsonArgs := []string{"tool", "test2json"}
	if pkg != "" {
		test2jsonArgs = append(test2jsonArgs, "-p", pkg)
	}
	test2json := exec.Command("go", test2jsonArgs...)
	test2json.Stdin = pipeR
	test2json.Stderr = os.Stderr

	jsonOut, err := test2json.StdoutPipe()
	if err != nil {
		os.Stdout = origStdout
		pipeW.Close()
		pipeR.Close()
		return m.Run()
	}

	if err := test2json.Start(); err != nil {
		os.Stdout = origStdout
		pipeW.Close()
		pipeR.Close()
		return m.Run()
	}

	// Process JSON events in a goroutine, passing through
	// human-readable output to the original stdout.
	done := make(chan error, 1)
	go func() {
		opts := []gotest.Option{gotest.WithOutput(origStdout)}
		if lp := otel.LoggerProvider(ctx); lp != nil {
			opts = append(opts, gotest.WithLoggerProvider(lp))
		}
		done <- gotest.Run(ctx, jsonOut, tp, opts...)
	}()

	// Run the tests.
	exitCode := m.Run()

	// Close the write end so test2json gets EOF.
	pipeW.Close()

	// Wait for the pipeline to finish.
	<-done
	test2json.Wait()

	// Restore stdout for any final output.
	os.Stdout = origStdout

	return exitCode
}

// forceTest2JSON replaces any existing -test.v flag with -test.v=test2json.
func forceTest2JSON() {
	newArgs := []string{os.Args[0], "-test.v=test2json"}
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-test.v") {
			newArgs = append(newArgs, arg)
		}
	}
	os.Args = newArgs
}

// detectPackage tries to determine the package name from the test binary.
func detectPackage() string {
	if exe, err := os.Executable(); err == nil {
		base := strings.TrimSuffix(exe, ".test")
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			return base[idx+1:]
		}
		return base
	}
	return ""
}

// hasOTLPEndpoint returns true if any OTLP trace endpoint is configured.
func hasOTLPEndpoint() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}
