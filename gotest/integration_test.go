package gotest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/dagger/otel-go/gotest"
	"github.com/dagger/otel-go/oteltestctx"
	"github.com/dagger/testctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// mustEncode writes a test event to the JSON stream, failing the test on error.
func mustEncode(t *testing.T, enc *json.Encoder, ev gotest.TestEvent) {
	t.Helper()
	require.NoError(t, enc.Encode(ev))
}

// testBinaryPackage returns the import path of the package under test,
// matching what oteltestctx.detectTestPackage() returns at runtime.
func testBinaryPackage() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return strings.TrimSuffix(bi.Path, ".test")
}

// TestSpanContextPropagation is an integration test that verifies the full
// cross-process span context flow:
//
//  1. gotest.Run creates a test span and registers it in the registry
//  2. A socket client (simulating oteltest) requests the span context
//  3. The client creates a child span using the remote span context
//  4. The child span is correctly parented under the test span
func TestSpanContextPropagation(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	socketPath := filepath.Join(t.TempDir(), "span.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	// Start a socket server mirroring otelgotest's serveSpanContexts.
	go serveSpanContexts(listener, registry)

	// Feed JSON events to gotest.Run via a pipe.
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp,
			gotest.WithSpanContextRegistry(registry),
		)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: "example.com/pkg"})
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestFoo"})

	// Simulate what oteltest does: connect to the socket and retrieve
	// the span context for TestFoo. The socket protocol uses
	// package-qualified names ("package/TestName").
	remoteSC := requestSpanContext(t, socketPath, "example.com/pkg/TestFoo")

	// Create a child span using the remote span context, simulating
	// an instrumented operation inside the test (e.g. a Dagger call).
	remoteCtx := trace.ContextWithRemoteSpanContext(context.Background(), remoteSC)
	_, childSpan := tp.Tracer("test-instrumentation").Start(remoteCtx, "child-operation")
	childSpan.End()

	// Finish the test.
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestFoo"})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg"})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)

	spans := spanRecorder.Ended()

	testSpan := findSpan(spans, "TestFoo")
	require.NotNil(t, testSpan, "expected TestFoo span")

	childSpanRO := findSpan(spans, "child-operation")
	require.NotNil(t, childSpanRO, "expected child-operation span")

	// The child should descend from the test span.
	assert.Equal(t, testSpan.SpanContext().TraceID(), childSpanRO.SpanContext().TraceID(),
		"child should share trace ID with test span")
	assert.Equal(t, testSpan.SpanContext().SpanID(), childSpanRO.Parent().SpanID(),
		"child should be parented under test span")
}

// TestSpanContextPropagationSubtest verifies that subtests also get the
// correct span context, parented under their parent test's span.
func TestSpanContextPropagationSubtest(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	socketPath := filepath.Join(t.TempDir(), "span.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	go serveSpanContexts(listener, registry)

	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp,
			gotest.WithSpanContextRegistry(registry),
		)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: "example.com/pkg"})
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestParent"})
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestParent/sub"})

	// Retrieve span contexts for both the parent and subtest.
	// Socket protocol uses package-qualified names.
	parentSC := requestSpanContext(t, socketPath, "example.com/pkg/TestParent")
	subSC := requestSpanContext(t, socketPath, "example.com/pkg/TestParent/sub")

	// Create child operations under each.
	parentCtx := trace.ContextWithRemoteSpanContext(context.Background(), parentSC)
	_, parentChild := tp.Tracer("test").Start(parentCtx, "parent-op")
	parentChild.End()

	subCtx := trace.ContextWithRemoteSpanContext(context.Background(), subSC)
	_, subChild := tp.Tracer("test").Start(subCtx, "sub-op")
	subChild.End()

	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestParent/sub"})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestParent"})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg"})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)

	spans := spanRecorder.Ended()

	parentSpan := findSpan(spans, "TestParent")
	require.NotNil(t, parentSpan)
	subSpan := findSpan(spans, "sub")
	require.NotNil(t, subSpan)
	parentOp := findSpan(spans, "parent-op")
	require.NotNil(t, parentOp)
	subOp := findSpan(spans, "sub-op")
	require.NotNil(t, subOp)

	// All spans should share a trace ID.
	traceID := parentSpan.SpanContext().TraceID()
	assert.Equal(t, traceID, subSpan.SpanContext().TraceID())
	assert.Equal(t, traceID, parentOp.SpanContext().TraceID())
	assert.Equal(t, traceID, subOp.SpanContext().TraceID())

	// Subtest span should be a child of the parent test span.
	assert.Equal(t, parentSpan.SpanContext().SpanID(), subSpan.Parent().SpanID(),
		"subtest should be a child of parent test")

	// In-process operations should be children of their respective test spans.
	assert.Equal(t, parentSpan.SpanContext().SpanID(), parentOp.Parent().SpanID(),
		"parent-op should be a child of TestParent")
	assert.Equal(t, subSpan.SpanContext().SpanID(), subOp.Parent().SpanID(),
		"sub-op should be a child of TestParent/sub")
}

// TestSpanContextPropagationWaitFor verifies that the socket blocks
// until the span is registered, even when the client connects before
// gotest.Run has processed the "run" event.
func TestSpanContextPropagationWaitFor(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	socketPath := filepath.Join(t.TempDir(), "span.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	go serveSpanContexts(listener, registry)

	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp,
			gotest.WithSpanContextRegistry(registry),
		)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: "example.com/pkg"})

	// Connect to the socket BEFORE writing the "run" event.
	// The handler should block until the span is registered.
	result := make(chan trace.SpanContext, 1)
	go func() {
		result <- requestSpanContext(t, socketPath, "example.com/pkg/TestLate")
	}()

	// Give the socket client time to connect and block.
	time.Sleep(50 * time.Millisecond)

	// Now write the "run" event — this should unblock the socket handler.
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestLate"})

	select {
	case sc := <-result:
		assert.True(t, sc.IsValid(), "expected valid span context")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for span context")
	}

	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestLate"})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg"})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)
}

// TestSpanContextWithMiddleware is a full end-to-end test using the real
// oteltestctx.WithTracing middleware. It verifies that when OTEL_TEST_SOCKET
// is set, the middleware adopts the external span context and downstream
// spans are correctly parented.
func TestSpanContextWithMiddleware(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	registry := gotest.NewSpanContextRegistry()
	defer registry.Close()

	socketPath := filepath.Join(t.TempDir(), "span.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	go serveSpanContexts(listener, registry)

	// Set the socket env var before creating the middleware so it picks it up.
	t.Setenv("OTEL_TEST_SOCKET", socketPath)

	// The middleware detects the package via debug.ReadBuildInfo(), so the
	// synthetic JSON events must use the same package path for the
	// package-qualified registry keys to match.
	pkg := testBinaryPackage()
	require.NotEmpty(t, pkg, "could not detect test binary package")

	// Feed synthetic JSON events to gotest.Run.
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp,
			gotest.WithSpanContextRegistry(registry),
		)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: pkg})

	// Write the "run" event matching the test name that testctx will produce.
	fullTestName := t.Name() + "/inner"
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: pkg, Test: fullTestName})

	// Run the real middleware. WithTracing sees OTEL_TEST_SOCKET, connects
	// to the socket, and adopts the span context from gotest.Run.
	testctx.New(t, oteltestctx.WithTracing[*testing.T]()).Run("inner", func(ctx context.Context, t *testctx.T) {
		// Create a downstream span — this should be a child of the
		// gotest-created test span via the adopted remote context.
		_, span := tp.Tracer("app").Start(ctx, "downstream-call")
		span.End()
	})

	// Finish the synthetic test events.
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: pkg, Test: fullTestName})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: pkg})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)

	// Find the spans.
	spans := spanRecorder.Ended()

	gotestSpan := findSpanByTestCaseName(spans, fullTestName)
	require.NotNil(t, gotestSpan, "expected gotest span for %q", fullTestName)

	downstreamSpan := findSpan(spans, "downstream-call")
	require.NotNil(t, downstreamSpan, "expected downstream-call span")

	// The downstream span should be a child of the gotest span.
	assert.Equal(t, gotestSpan.SpanContext().TraceID(), downstreamSpan.SpanContext().TraceID(),
		"downstream should share trace ID with gotest span")
	assert.Equal(t, gotestSpan.SpanContext().SpanID(), downstreamSpan.Parent().SpanID(),
		"downstream should be parented under gotest span")
}

// TestFailureStatusIgnoresTestOutput verifies that verbose testify-style
// failure output does not become the span status description.
func TestFailureStatusIgnoresTestOutput(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: "example.com/pkg"})
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestVerbose"})

	// Simulate testify-formatted error output as test2json would emit it.
	// Go's testing.decorate adds "    file.go:line: " to the first line
	// and 4 spaces of indentation to continuation lines.
	for _, line := range []string{
		"    test.go:42: \n",
		"    \tError Trace:\t/src/test.go:42\n",
		"    \t            \t\t\t/src/helper.go:10\n",
		"    \tError:      \tExpected nil, but got error\n",
		"    \tTest:       \tTestVerbose\n",
	} {
		mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "output", Package: "example.com/pkg", Test: "TestVerbose", Output: line})
	}

	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "fail", Package: "example.com/pkg", Test: "TestVerbose", Elapsed: 0.01})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "fail", Package: "example.com/pkg"})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)

	spans := spanRecorder.Ended()
	span := findSpan(spans, "TestVerbose")
	require.NotNil(t, span)

	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "test failed", span.Status().Description,
		"span status should not contain dynamic test output")
}

// TestFailureStatusIgnoresNoisyTestOutput verifies that noisy test output does
// not become the span status description.
func TestFailureStatusIgnoresNoisyTestOutput(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- gotest.Run(t.Context(), pr, tp)
	}()

	now := time.Now()
	enc := json.NewEncoder(pw)
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "start", Package: "example.com/pkg"})
	mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestNoisy"})

	// Mix of regular log output and testify error.
	for _, line := range []string{
		"    test.go:10: some log output\n",
		"    test.go:20: more log output\n",
		"    test.go:42: \n",
		"    \tError Trace:\t/src/test.go:42\n",
		"    \tError:      \tCondition never satisfied\n",
		"    \tTest:       \tTestNoisy\n",
		"    test.go:50: cleanup: exit status 1\n",
		"    test.go:60: server: shutting down\n",
	} {
		mustEncode(t, enc, gotest.TestEvent{Time: now, Action: "output", Package: "example.com/pkg", Test: "TestNoisy", Output: line})
	}

	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "fail", Package: "example.com/pkg", Test: "TestNoisy", Elapsed: 0.01})
	mustEncode(t, enc, gotest.TestEvent{Time: now.Add(time.Second), Action: "fail", Package: "example.com/pkg"})
	require.NoError(t, pw.Close())

	require.NoError(t, <-done)

	spans := spanRecorder.Ended()
	span := findSpan(spans, "TestNoisy")
	require.NotNil(t, span)

	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "test failed", span.Status().Description,
		"span status should not contain dynamic test output")
}

// --- helpers ---

// serveSpanContexts mirrors otelgotest's socket server.
func serveSpanContexts(listener net.Listener, registry *gotest.SpanContextRegistry) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			scanner := bufio.NewScanner(conn)
			if !scanner.Scan() {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sc, ok := registry.WaitFor(ctx, scanner.Text())
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(conn, "00-%s-%s-%s\n", sc.TraceID(), sc.SpanID(), sc.TraceFlags())
		}()
	}
}

// requestSpanContext connects to the socket and retrieves a span context.
func requestSpanContext(t *testing.T, socketPath, testName string) trace.SpanContext {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	_, err = fmt.Fprintln(conn, testName)
	require.NoError(t, err)

	scanner := bufio.NewScanner(conn)
	require.True(t, scanner.Scan(), "expected traceparent response from socket")

	prop := propagation.TraceContext{}
	ctx := prop.Extract(context.Background(), propagation.MapCarrier{
		"traceparent": scanner.Text(),
	})
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "invalid traceparent: %s", scanner.Text())
	return sc
}
