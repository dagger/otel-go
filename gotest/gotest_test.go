package gotest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	otel "github.com/dagger/otel-go"
	"github.com/dagger/otel-go/gotest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

func runFixture(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()

	f, err := os.Open("testdata/sample.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	ctx := t.Context()
	err = gotest.Run(ctx, f, tp)
	require.NoError(t, err)

	return spanRecorder.Ended()
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func findSpanByTestCaseName(spans []sdktrace.ReadOnlySpan, testName string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if spanAttr(s, semconv.TestCaseNameKey).AsString() == testName {
			return s
		}
	}
	return nil
}

func findSpansByTestCaseName(spans []sdktrace.ReadOnlySpan, testName string) []sdktrace.ReadOnlySpan {
	var matches []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if spanAttr(s, semconv.TestCaseNameKey).AsString() == testName {
			matches = append(matches, s)
		}
	}
	return matches
}

func findContinuationSpanFor(spans []sdktrace.ReadOnlySpan, testSpan sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() != "continue" {
			continue
		}
		for _, link := range s.Links() {
			if link.SpanContext.TraceID() == testSpan.SpanContext().TraceID() &&
				link.SpanContext.SpanID() == testSpan.SpanContext().SpanID() {
				return s
			}
		}
	}
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

type recordingLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *recordingLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range records {
		e.records = append(e.records, rec.Clone())
	}
	return nil
}

func (e *recordingLogExporter) Shutdown(context.Context) error   { return nil }
func (e *recordingLogExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingLogExporter) Records() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	records := make([]sdklog.Record, len(e.records))
	copy(records, e.records)
	return records
}

func runEvents(t *testing.T, events []gotest.TestEvent, opts ...gotest.Option) []sdktrace.ReadOnlySpan {
	t.Helper()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		require.NoError(t, enc.Encode(ev))
	}

	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	err := gotest.Run(t.Context(), &buf, tp, opts...)
	require.NoError(t, err)

	return spanRecorder.Ended()
}

func nestedTestEvents(pkg string, chain []string, leaf string) []gotest.TestEvent {
	now := time.Now()
	events := []gotest.TestEvent{{Time: now, Action: "start", Package: pkg}}

	var full string
	ancestors := make([]string, 0, len(chain))
	for _, name := range chain {
		if full == "" {
			full = name
		} else {
			full += "/" + name
		}
		ancestors = append(ancestors, full)
		events = append(events, gotest.TestEvent{Time: now, Action: "run", Package: pkg, Test: full})
	}

	fullLeaf := full + "/" + leaf
	events = append(events,
		gotest.TestEvent{Time: now, Action: "run", Package: pkg, Test: fullLeaf},
		gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: pkg, Test: fullLeaf},
	)

	for i := len(ancestors) - 1; i >= 0; i-- {
		events = append(events, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: pkg, Test: ancestors[i]})
	}
	return append(events, gotest.TestEvent{Time: now.Add(time.Second), Action: "pass", Package: pkg})
}

func spanAttr(span sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	for _, a := range span.Attributes() {
		if a.Key == key {
			return a.Value
		}
	}
	return attribute.Value{}
}

func TestPassingTest(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestPass")
	require.NotNil(t, span, "expected span for TestPass")

	assert.Equal(t, codes.Ok, span.Status().Code)

	// semconv attributes
	assert.Equal(t, "TestPass",
		spanAttr(span, semconv.TestCaseNameKey).AsString())

	// result status
	assert.Contains(t, span.Attributes(), semconv.TestCaseResultStatusPass)

	// duration should be > 0 (test sleeps 10ms)
	assert.True(t, span.EndTime().After(span.StartTime()))
}

func TestFailingTest(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestFail")
	require.NotNil(t, span, "expected span for TestFail")

	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "test failed", span.Status().Description)

	// result status
	assert.Contains(t, span.Attributes(), semconv.TestSuiteRunStatusFailure)
}

func TestSkippedTest(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestSkip")
	require.NotNil(t, span, "expected span for TestSkip")

	assert.Equal(t, codes.Ok, span.Status().Code)

	// result status
	assert.Contains(t, span.Attributes(), semconv.TestSuiteRunStatusSkipped)
}

func TestSubtestNesting(t *testing.T) {
	spans := runFixture(t)

	parent := findSpan(spans, "TestSub")
	require.NotNil(t, parent, "expected span for TestSub")

	child := findSpan(spans, "level1")
	require.NotNil(t, child, "expected span for level1")

	grandchild := findSpan(spans, "level2")
	require.NotNil(t, grandchild, "expected span for level2")

	// Verify parent-child relationships
	assert.Equal(t, parent.SpanContext().SpanID(), child.Parent().SpanID(),
		"level1 should be a child of TestSub")
	assert.Equal(t, child.SpanContext().SpanID(), grandchild.Parent().SpanID(),
		"level2 should be a child of level1")

	// Span names should be base names
	assert.Equal(t, "TestSub", parent.Name())
	assert.Equal(t, "level1", child.Name())
	assert.Equal(t, "level2", grandchild.Name())

	// But test.case.name should be the full path
	assert.Equal(t, "TestSub/level1",
		spanAttr(child, semconv.TestCaseNameKey).AsString())
	assert.Equal(t, "TestSub/level1/level2",
		spanAttr(grandchild, semconv.TestCaseNameKey).AsString())
}

// Subtest leaf names can themselves contain '/'. For example, a parent test
// may call t.Run("https://github.com/dagger/dagger.git:", ...) or
// t.Run("sub/a_-_../b", ...). Those embedded slashes are part of the leaf
// name, not extra levels of nesting.
func TestSubtestLeafNamesContainingSlashes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		chain []string
		leaf  string
	}{
		{
			name:  "url-like leaf with .git:",
			chain: []string{"TestCall", "TestArgTypes", "directory_arg_inputs", "git_dir"},
			leaf:  "https://github.com/dagger/dagger.git:",
		},
		{
			name:  "url-like leaf with .git:.changes",
			chain: []string{"TestCall", "TestArgTypes", "directory_arg_inputs", "git_dir"},
			leaf:  "https://github.com/dagger/dagger.git:.changes",
		},
		{
			name:  "url-like leaf with :",
			chain: []string{"TestCall", "TestArgTypes", "directory_arg_inputs", "git_dir"},
			leaf:  "https://github.com/dagger/dagger:",
		},
		{
			name:  "url-like leaf with :.changes",
			chain: []string{"TestCall", "TestArgTypes", "directory_arg_inputs", "git_dir"},
			leaf:  "https://github.com/dagger/dagger:.changes",
		},
		{
			name:  "prefixed dot segment leaf",
			chain: []string{"TestShell", "TestDirectoryFlag"},
			leaf:  "._-_sub/a",
		},
		{
			name:  "suffix dot segment leaf",
			chain: []string{"TestShell", "TestDirectoryFlag"},
			leaf:  "sub/a_-_.",
		},
		{
			name:  "parent traversal leaf",
			chain: []string{"TestShell", "TestDirectoryFlag"},
			leaf:  "sub/a_-_../b",
		},
		{
			name:  "double parent traversal leaf",
			chain: []string{"TestShell", "TestDirectoryFlag"},
			leaf:  "sub/a_-_../../ab",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spans := runEvents(t, nestedTestEvents("example.com/pkg", tc.chain, tc.leaf))

			parent := findSpan(spans, tc.chain[len(tc.chain)-1])
			require.NotNil(t, parent, "expected span for parent %q", tc.chain[len(tc.chain)-1])

			leaf := findSpan(spans, tc.leaf)
			require.NotNilf(t, leaf, "expected span name to preserve full leaf %q; got spans %v", tc.leaf, spanNames(spans))

			assert.Equal(t, parent.SpanContext().SpanID(), leaf.Parent().SpanID(),
				"leaf span should stay nested under %q even when its name contains '/'", tc.chain[len(tc.chain)-1])

			fullName := strings.Join(append(append([]string{}, tc.chain...), tc.leaf), "/")
			assert.Equal(t, fullName, spanAttr(leaf, semconv.TestCaseNameKey).AsString())
		})
	}
}

func TestSiblingLeafCanContainAnotherSubtestName(t *testing.T) {
	t.Parallel()

	now := time.Now()
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: "example.com/pkg"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestAmbig"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestAmbig/B"},
		{Time: now, Action: "pause", Package: "example.com/pkg", Test: "TestAmbig/B"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestAmbig/B/C"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestAmbig/B/C"},
		{Time: now.Add(time.Second), Action: "cont", Package: "example.com/pkg", Test: "TestAmbig/B"},
		{Time: now.Add(2 * time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestAmbig/B"},
		{Time: now.Add(2 * time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestAmbig"},
		{Time: now.Add(2 * time.Second), Action: "pass", Package: "example.com/pkg"},
	})

	parent := findSpanByTestCaseName(spans, "TestAmbig")
	require.NotNil(t, parent)

	b := findSpanByTestCaseName(spans, "TestAmbig/B")
	require.NotNil(t, b)
	assert.Equal(t, "B", b.Name())
	assert.Equal(t, parent.SpanContext().SpanID(), b.Parent().SpanID())

	bc := findSpanByTestCaseName(spans, "TestAmbig/B/C")
	require.NotNil(t, bc)
	assert.Equal(t, "B/C", bc.Name())
	assert.Equal(t, parent.SpanContext().SpanID(), bc.Parent().SpanID(),
		"TestAmbig/B/C should be a sibling of TestAmbig/B, not its child")
}

func TestInterleavedParallelSubtestsPreserveParents(t *testing.T) {
	t.Parallel()

	now := time.Now()
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: "example.com/pkg"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestPar"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestPar/A"},
		{Time: now, Action: "pause", Package: "example.com/pkg", Test: "TestPar/A"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestPar/B"},
		{Time: now, Action: "pause", Package: "example.com/pkg", Test: "TestPar/B"},
		{Time: now, Action: "cont", Package: "example.com/pkg", Test: "TestPar/A"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestPar/A/child"},
		{Time: now, Action: "cont", Package: "example.com/pkg", Test: "TestPar/B"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestPar/B/child"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestPar/A/child"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestPar/A"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestPar/B/child"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestPar/B"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestPar"},
		{Time: now.Add(time.Second), Action: "pass", Package: "example.com/pkg"},
	})

	a := findSpanByTestCaseName(spans, "TestPar/A")
	require.NotNil(t, a)
	aCont := findContinuationSpanFor(spans, a)
	require.NotNil(t, aCont)
	b := findSpanByTestCaseName(spans, "TestPar/B")
	require.NotNil(t, b)
	bCont := findContinuationSpanFor(spans, b)
	require.NotNil(t, bCont)

	aChild := findSpanByTestCaseName(spans, "TestPar/A/child")
	require.NotNil(t, aChild)
	assert.Equal(t, "child", aChild.Name())
	assert.Equal(t, a.SpanContext().SpanID(), aChild.Parent().SpanID(),
		"child of parallel test should be parented by original span, not continuation")
	assert.NotEqual(t, aCont.SpanContext().SpanID(), aChild.Parent().SpanID())

	bChild := findSpanByTestCaseName(spans, "TestPar/B/child")
	require.NotNil(t, bChild)
	assert.Equal(t, "child", bChild.Name())
	assert.Equal(t, b.SpanContext().SpanID(), bChild.Parent().SpanID(),
		"child of parallel test should be parented by original span, not continuation")
	assert.NotEqual(t, bCont.SpanContext().SpanID(), bChild.Parent().SpanID())
}

func TestElapsedDeterminesEndTime(t *testing.T) {
	t.Parallel()

	startAt := time.Now()
	passEventAt := startAt.Add(10 * time.Second)
	expectedEnd := startAt.Add(250 * time.Millisecond)

	spans := runEvents(t, []gotest.TestEvent{
		{Time: startAt, Action: "start", Package: "example.com/pkg"},
		{Time: startAt, Action: "run", Package: "example.com/pkg", Test: "TestFast"},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Test: "TestFast", Elapsed: 0.25},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Elapsed: 10},
	})

	span := findSpanByTestCaseName(spans, "TestFast")
	require.NotNil(t, span)
	assert.True(t, span.StartTime().Equal(startAt))
	assert.True(t, span.EndTime().Equal(expectedEnd),
		"span should end at start+Elapsed, not the late pass event timestamp")
}

func TestPauseContSplitsSpanAndLinks(t *testing.T) {
	t.Parallel()

	startAt := time.Now()
	runAt := startAt.Add(time.Second)
	pauseAt := startAt.Add(2 * time.Second)
	contAt := startAt.Add(10 * time.Second)
	endAt := contAt.Add(time.Second)
	passEventAt := startAt.Add(20 * time.Second)

	spans := runEvents(t, []gotest.TestEvent{
		{Time: startAt, Action: "start", Package: "example.com/pkg"},
		{Time: runAt, Action: "run", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: pauseAt, Action: "pause", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: contAt, Action: "cont", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Test: "TestParallel", Elapsed: 1},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Elapsed: 20},
	})

	pkg := findSpan(spans, "example.com/pkg")
	require.NotNil(t, pkg)

	testSpans := findSpansByTestCaseName(spans, "TestParallel")
	require.Len(t, testSpans, 1)
	setup := testSpans[0]
	cont := findContinuationSpanFor(spans, setup)
	require.NotNil(t, cont)

	assert.True(t, setup.StartTime().Equal(runAt))
	assert.True(t, setup.EndTime().Equal(pauseAt))
	assert.Equal(t, codes.Unset, setup.Status().Code)
	assert.Equal(t, pkg.SpanContext().SpanID(), setup.Parent().SpanID())

	assert.Equal(t, "continue", cont.Name())
	assert.Equal(t, "", spanAttr(cont, semconv.TestCaseNameKey).AsString())
	assert.True(t, cont.StartTime().Equal(contAt))
	assert.True(t, cont.EndTime().Equal(endAt))
	assert.Equal(t, codes.Ok, cont.Status().Code)
	assert.Equal(t, setup.SpanContext().SpanID(), cont.Parent().SpanID())
	assert.True(t, spanAttr(cont, attribute.Key(otel.UIPassthroughAttr)).AsBool())

	require.Len(t, cont.Links(), 1)
	assert.Equal(t, setup.SpanContext().TraceID(), cont.Links()[0].SpanContext.TraceID())
	assert.Equal(t, setup.SpanContext().SpanID(), cont.Links()[0].SpanContext.SpanID())
}

func TestPauseContLogsStayOnOriginalSpan(t *testing.T) {
	t.Parallel()

	logExporter := &recordingLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))
	t.Cleanup(func() {
		require.NoError(t, lp.Shutdown(context.Background()))
	})

	now := time.Now()
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: "example.com/pkg"},
		{Time: now, Action: "run", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: now, Action: "pause", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: now.Add(time.Second), Action: "cont", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: now.Add(time.Second), Action: "output", Package: "example.com/pkg", Test: "TestParallel", Output: "continued output\n"},
		{Time: now.Add(2 * time.Second), Action: "pass", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: now.Add(2 * time.Second), Action: "pass", Package: "example.com/pkg"},
	}, gotest.WithLoggerProvider(lp))
	require.NoError(t, lp.ForceFlush(context.Background()))

	testSpans := findSpansByTestCaseName(spans, "TestParallel")
	require.Len(t, testSpans, 1)
	setup := testSpans[0]
	cont := findContinuationSpanFor(spans, setup)
	require.NotNil(t, cont)

	var outputLog *sdklog.Record
	for _, rec := range logExporter.Records() {
		if rec.Body().AsString() == "continued output\n" {
			rec := rec
			outputLog = &rec
			break
		}
	}
	require.NotNil(t, outputLog, "expected continued output log record")
	assert.Equal(t, setup.SpanContext().TraceID(), outputLog.TraceID())
	assert.Equal(t, setup.SpanContext().SpanID(), outputLog.SpanID())
	assert.NotEqual(t, cont.SpanContext().SpanID(), outputLog.SpanID(),
		"continued output should stay routed to the original pre-PAUSE span")
}

func TestPackageSpan(t *testing.T) {
	spans := runFixture(t)

	pkg := findSpan(spans, "github.com/dagger/otel-go/gotest/testdata/sample")
	require.NotNil(t, pkg, "expected package span")

	// Package has a failure so it should be marked as error.
	assert.Equal(t, codes.Error, pkg.Status().Code)
	assert.Contains(t, pkg.Attributes(), semconv.TestSuiteRunStatusFailure)

	// Top-level tests should be children of the package span.
	pass := findSpan(spans, "TestPass")
	require.NotNil(t, pass)
	assert.Equal(t, pkg.SpanContext().SpanID(), pass.Parent().SpanID(),
		"TestPass should be a child of the package span")
}

func TestParallelTests(t *testing.T) {
	spans := runFixture(t)

	parent := findSpan(spans, "TestParallel1")
	require.NotNil(t, parent, "expected span for TestParallel1")

	aSpans := findSpansByTestCaseName(spans, "TestParallel1/a")
	require.Len(t, aSpans, 1, "parallel/a should have a single test span")
	a := aSpans[0]
	aCont := findContinuationSpanFor(spans, a)
	require.NotNil(t, aCont, "expected continuation span for parallel/a")

	bSpans := findSpansByTestCaseName(spans, "TestParallel1/b")
	require.Len(t, bSpans, 1, "parallel/b should have a single test span")
	b := bSpans[0]
	bCont := findContinuationSpanFor(spans, b)
	require.NotNil(t, bCont, "expected continuation span for parallel/b")

	// Test spans should be children of TestParallel1.
	assert.Equal(t, parent.SpanContext().SpanID(), a.Parent().SpanID())
	assert.Equal(t, parent.SpanContext().SpanID(), b.Parent().SpanID())

	// Continuation spans should be passthrough children of their test spans.
	assert.Equal(t, "continue", aCont.Name())
	assert.Equal(t, "continue", bCont.Name())
	assert.Equal(t, "", spanAttr(aCont, semconv.TestCaseNameKey).AsString())
	assert.Equal(t, "", spanAttr(bCont, semconv.TestCaseNameKey).AsString())
	assert.Equal(t, a.SpanContext().SpanID(), aCont.Parent().SpanID())
	assert.Equal(t, b.SpanContext().SpanID(), bCont.Parent().SpanID())
	assert.True(t, spanAttr(aCont, attribute.Key(otel.UIPassthroughAttr)).AsBool())
	assert.True(t, spanAttr(bCont, attribute.Key(otel.UIPassthroughAttr)).AsBool())

	// Both should pass.
	assert.Equal(t, codes.Ok, aCont.Status().Code)
	assert.Equal(t, codes.Ok, bCont.Status().Code)

	// The continuation spans should link back to the setup spans that ended at PAUSE.
	require.Len(t, aCont.Links(), 1)
	assert.Equal(t, a.SpanContext().SpanID(), aCont.Links()[0].SpanContext.SpanID())
	assert.Equal(t, codes.Unset, a.Status().Code)
	assert.False(t, a.EndTime().After(aCont.StartTime()))

	require.Len(t, bCont.Links(), 1)
	assert.Equal(t, b.SpanContext().SpanID(), bCont.Links()[0].SpanContext.SpanID())
	assert.Equal(t, codes.Unset, b.Status().Code)
	assert.False(t, b.EndTime().After(bCont.StartTime()))
}

func TestSkippedPackage(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	// Simulate what go test -json emits for a package with no test files:
	//   {"Action":"start","Package":"example.com/nopkg"}
	//   {"Action":"output","Package":"example.com/nopkg","Output":"? \texample.com/nopkg\t[no test files]\n"}
	//   {"Action":"skip","Package":"example.com/nopkg"}
	events := `{"Time":"2025-01-01T00:00:00Z","Action":"start","Package":"example.com/nopkg"}
{"Time":"2025-01-01T00:00:00Z","Action":"output","Package":"example.com/nopkg","Output":"? \texample.com/nopkg\t[no test files]\n"}
{"Time":"2025-01-01T00:00:00Z","Action":"skip","Package":"example.com/nopkg"}
`

	err := gotest.Run(t.Context(), strings.NewReader(events), tp)
	require.NoError(t, err)

	spans := spanRecorder.Ended()
	require.Len(t, spans, 1, "expected the skipped package span to be ended")

	pkg := spans[0]
	assert.Equal(t, "example.com/nopkg", pkg.Name())
	assert.Equal(t, codes.Ok, pkg.Status().Code)
	assert.Contains(t, pkg.Attributes(), semconv.TestSuiteRunStatusSkipped)
}

func TestPackageWithNoTestsToRunIsSkipped(t *testing.T) {
	t.Parallel()

	// go test -run NoSuchTest still reports the package as passing, printing
	// "[no tests to run]" - it must not be reported as a passing suite.
	now := time.Now()
	pkg := "example.com/pkg"
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: pkg},
		{Time: now, Action: "output", Package: pkg, Output: "testing: warning: no tests to run\n"},
		{Time: now, Action: "output", Package: pkg, Output: "ok \texample.com/pkg\t0.001s [no tests to run]\n"},
		{Time: now, Action: "pass", Package: pkg},
	})

	pkgSpan := findSpan(spans, pkg)
	require.NotNil(t, pkgSpan)
	assert.Equal(t, codes.Ok, pkgSpan.Status().Code)
	assert.Contains(t, pkgSpan.Attributes(), semconv.TestSuiteRunStatusSkipped,
		"a package that ran no tests must not be reported as passing")
	assert.NotContains(t, pkgSpan.Attributes(), semconv.TestCaseResultStatusPass)
}

// Output that belongs to no test, such as a TestMain teardown failure after
// m.Run, is logged on the package span even without WithOutput, as when
// the caller consumes the JSON stream.
func TestPackageOutputIsLoggedOnPackageSpan(t *testing.T) {
	t.Parallel()

	logExporter := &recordingLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))
	t.Cleanup(func() {
		require.NoError(t, lp.Shutdown(context.Background()))
	})

	now := time.Now()
	pkg := "example.com/pkg"
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: pkg},
		{Time: now, Action: "run", Package: pkg, Test: "TestOK"},
		{Time: now, Action: "output", Package: pkg, Test: "TestOK", Output: "=== RUN   TestOK\n"},
		{Time: now, Action: "output", Package: pkg, Test: "TestOK", Output: "--- PASS: TestOK (0.00s)\n"},
		{Time: now, Action: "pass", Package: pkg, Test: "TestOK"},
		{Time: now, Action: "output", Package: pkg, Output: "PASS\n"},
		{Time: now, Action: "output", Package: pkg, Output: "teardown failure: database still running\n"},
		{Time: now, Action: "output", Package: pkg, Output: "FAIL\texample.com/pkg\t0.010s\n"},
		{Time: now, Action: "fail", Package: pkg},
	}, gotest.WithLoggerProvider(lp))
	require.NoError(t, lp.ForceFlush(context.Background()))

	pkgSpan := findSpan(spans, pkg)
	require.NotNil(t, pkgSpan)
	assert.Equal(t, codes.Error, pkgSpan.Status().Code)
	assert.Equal(t,
		"teardown failure: database still running\nFAIL\texample.com/pkg\t0.010s\n",
		logsOn(logExporter.Records(), pkgSpan))

	test := findSpanByTestCaseName(spans, "TestOK")
	require.NotNil(t, test)
	assert.Empty(t, logsOn(logExporter.Records(), test), "package output stays off the test span")
}

// buildEvent returns a go test -json event reporting on the build id, as
// go test emits for a build's output and failure since Go 1.24.
func buildEvent(now time.Time, id, action, output string) gotest.TestEvent {
	return gotest.TestEvent{Time: now, Action: action, ImportPath: id, Output: output}
}

// A package whose test binary does not build has its compile errors,
// reported by build-output events before the package starts, logged on its
// span ahead of its FAIL line, and nowhere else.
func TestBuildFailureIsLoggedOnPackageSpan(t *testing.T) {
	t.Parallel()

	now := time.Now()
	legacy := "example.com/shop/internal/legacy"
	cart := "example.com/shop/cart"
	id := legacy + " [" + legacy + ".test]"
	compileErrors := "# " + id + "\n" +
		"internal/legacy/legacy.go:12:2: undefined: oldClient\n" +
		"internal/legacy/legacy.go:19:9: cannot use v (variable of type int) as string value in return statement\n"
	failLine := "FAIL\t" + legacy + " [build failed]\n"

	spans, out, logs := runWithOutput(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: cart},
		{Time: now, Action: "run", Package: cart, Test: "TestCart"},
		{Time: now, Action: "output", Package: cart, Test: "TestCart", Output: "=== RUN   TestCart\n"},
		buildEvent(now, id, "build-output", "# "+id+"\n"),
		buildEvent(now, id, "build-output", "internal/legacy/legacy.go:12:2: undefined: oldClient\n"),
		buildEvent(now, id, "build-output", "internal/legacy/legacy.go:19:9: cannot use v (variable of type int) as string value in return statement\n"),
		buildEvent(now, id, "build-fail", ""),
		{Time: now, Action: "output", Package: cart, Test: "TestCart", Output: "cart_test.go:8: checking out\n"},
		{Time: now, Action: "output", Package: cart, Test: "TestCart", Output: "--- PASS: TestCart (0.00s)\n"},
		{Time: now, Action: "pass", Package: cart, Test: "TestCart"},
		{Time: now, Action: "start", Package: legacy},
		{Time: now, Action: "output", Package: legacy, Output: failLine},
		{Time: now, Action: "fail", Package: legacy, FailedBuild: id},
		{Time: now, Action: "output", Package: cart, Output: "PASS\n"},
		{Time: now, Action: "output", Package: cart, Output: "ok  \t" + cart + "\t0.010s\n"},
		{Time: now, Action: "pass", Package: cart},
	})

	legacySpan := findSpan(spans, legacy)
	require.NotNil(t, legacySpan)
	assert.Equal(t, codes.Error, legacySpan.Status().Code)
	assert.Equal(t, compileErrors+failLine, logsOn(logs.Records(), legacySpan))

	cartSpan := findSpan(spans, cart)
	require.NotNil(t, cartSpan)
	assert.Equal(t, "ok  \t"+cart+"\t0.010s\n", logsOn(logs.Records(), cartSpan))
	test := findSpanByTestCaseName(spans, "TestCart")
	require.NotNil(t, test)
	assert.Equal(t, "cart_test.go:8: checking out\n", logsOn(logs.Records(), test),
		"build output stays off test spans")

	// The human-readable output is what go test prints.
	assert.Equal(t, compileErrors+failLine+"ok  \t"+cart+"\t0.010s\n", out)
}

// The builds of a package's test binary are named with a " [pkg.test]"
// suffix: the package itself, its external _test package, and dependencies
// recompiled for the test; its link step is named "pkg.test". Their output,
// such as what -gcflags=-m prints, is logged on the package span whether
// the build fails or not, and whether it arrives before the package starts
// or after.
func TestBuildOutputOfTestBinaryIsLoggedOnPackageSpan(t *testing.T) {
	t.Parallel()

	pkg := "example.com/shop/internal/legacy"
	for name, id := range map[string]string{
		"package":               pkg + " [" + pkg + ".test]",
		"external test":         pkg + "_test [" + pkg + ".test]",
		"recompiled dependency": "example.com/shop/internal/db [" + pkg + ".test]",
		"test binary link":      pkg + ".test",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			now := time.Now()
			spans, _, logs := runWithOutput(t, []gotest.TestEvent{
				buildEvent(now, id, "build-output", "# "+id+"\n"),
				buildEvent(now, id, "build-output", "./legacy.go:3:6: can inline F\n"),
				{Time: now, Action: "start", Package: pkg},
				buildEvent(now, id, "build-output", "./legacy.go:7:6: can inline G\n"),
				{Time: now, Action: "run", Package: pkg, Test: "TestF"},
				{Time: now, Action: "pass", Package: pkg, Test: "TestF"},
				{Time: now, Action: "output", Package: pkg, Output: "ok  \t" + pkg + "\t0.010s\n"},
				{Time: now, Action: "pass", Package: pkg},
			})

			pkgSpan := findSpan(spans, pkg)
			require.NotNil(t, pkgSpan)
			assert.Equal(t,
				"# "+id+"\n./legacy.go:3:6: can inline F\n./legacy.go:7:6: can inline G\nok  \t"+pkg+"\t0.010s\n",
				logsOn(logs.Records(), pkgSpan))
			test := findSpanByTestCaseName(spans, "TestF")
			require.NotNil(t, test)
			assert.Empty(t, logsOn(logs.Records(), test))
		})
	}
}

// A package whose only tests are external is compiled as is, under its own
// ImportPath, as a dependency of its tests would be; when it fails, its
// output is logged from the fail event that names it, ahead of the FAIL
// line, whenever it arrived.
func TestBuildFailureOfPackageBuiltAsIsIsLoggedOnPackageSpan(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pkg := "example.com/shop/internal/legacy"
	failLine := "FAIL\t" + pkg + " [build failed]\n"
	spans, _, logs := runWithOutput(t, []gotest.TestEvent{
		buildEvent(now, pkg, "build-output", "# "+pkg+"\n"),
		buildEvent(now, pkg, "build-output", "internal/legacy/legacy.go:3:26: undefined: oldClient\n"),
		{Time: now, Action: "start", Package: pkg},
		buildEvent(now, pkg, "build-output", "internal/legacy/legacy.go:4:26: undefined: newClient\n"),
		buildEvent(now, pkg, "build-fail", ""),
		{Time: now, Action: "output", Package: pkg, Output: failLine},
		{Time: now, Action: "fail", Package: pkg, FailedBuild: pkg},
	})

	pkgSpan := findSpan(spans, pkg)
	require.NotNil(t, pkgSpan)
	assert.Equal(t,
		"# "+pkg+"\ninternal/legacy/legacy.go:3:26: undefined: oldClient\ninternal/legacy/legacy.go:4:26: undefined: newClient\n"+failLine,
		logsOn(logs.Records(), pkgSpan))
}

// A package with internal tests that fails to build, and that another
// package under test imports, is built twice: as is, for the importer, and
// with its tests. Each package logs the build that failed it.
func TestPackageAndItsTestVariantBuildsAreLoggedApart(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dep := "example.com/shop/dep"
	depTest := dep + " [" + dep + ".test]"
	api := "example.com/shop/api"
	depErrors := "# " + dep + "\ndep/dep.go:3:26: undefined: legacyConfig\n"
	depTestErrors := "# " + depTest + "\ndep/dep.go:3:26: undefined: legacyConfig\n"

	var events []gotest.TestEvent
	for _, b := range []struct{ id, output string }{{dep, depErrors}, {depTest, depTestErrors}} {
		for line := range strings.Lines(b.output) {
			events = append(events, buildEvent(now, b.id, "build-output", line))
		}
		events = append(events, buildEvent(now, b.id, "build-fail", ""))
	}
	for _, p := range []struct{ pkg, failedBuild string }{{api, dep}, {dep, depTest}} {
		events = append(events,
			gotest.TestEvent{Time: now, Action: "start", Package: p.pkg},
			gotest.TestEvent{Time: now, Action: "output", Package: p.pkg, Output: "FAIL\t" + p.pkg + " [build failed]\n"},
			gotest.TestEvent{Time: now, Action: "fail", Package: p.pkg, FailedBuild: p.failedBuild},
		)
	}
	spans, _, logs := runWithOutput(t, events)

	apiSpan := findSpan(spans, api)
	require.NotNil(t, apiSpan)
	assert.Equal(t, depErrors+"FAIL\t"+api+" [build failed]\n", logsOn(logs.Records(), apiSpan))
	depSpan := findSpan(spans, dep)
	require.NotNil(t, depSpan)
	assert.Equal(t, depTestErrors+"FAIL\t"+dep+" [build failed]\n", logsOn(logs.Records(), depSpan))
}

// A dependency that fails to build is named by its own ImportPath and fails
// every package under test that imports it; each of them, which name it as
// their FailedBuild, logs its output once, and no other package does.
func TestDependencyBuildFailureIsLoggedOnFailedPackages(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dep := "example.com/shop/internal/dep"
	api := "example.com/shop/api"
	web := "example.com/shop/web"
	cart := "example.com/shop/cart"
	compileErrors := "# " + dep + "\ninternal/dep/dep.go:7:2: undefined: legacyConfig\n"
	failed := func(pkg string) []gotest.TestEvent {
		return []gotest.TestEvent{
			{Time: now, Action: "start", Package: pkg},
			{Time: now, Action: "output", Package: pkg, Output: "FAIL\t" + pkg + " [build failed]\n"},
			{Time: now, Action: "fail", Package: pkg, FailedBuild: dep},
		}
	}

	events := []gotest.TestEvent{
		buildEvent(now, dep, "build-output", "# "+dep+"\n"),
		buildEvent(now, dep, "build-output", "internal/dep/dep.go:7:2: undefined: legacyConfig\n"),
		buildEvent(now, dep, "build-fail", ""),
	}
	events = append(events, failed(api)...)
	events = append(events, failed(web)...)
	// The dependency is under test too, without test files.
	events = append(events, failed(dep)...)
	events = append(events,
		gotest.TestEvent{Time: now, Action: "start", Package: cart},
		gotest.TestEvent{Time: now, Action: "run", Package: cart, Test: "TestCart"},
		gotest.TestEvent{Time: now, Action: "pass", Package: cart, Test: "TestCart"},
		gotest.TestEvent{Time: now, Action: "output", Package: cart, Output: "ok  \t" + cart + "\t0.010s\n"},
		gotest.TestEvent{Time: now, Action: "pass", Package: cart},
	)
	spans, out, logs := runWithOutput(t, events)

	for _, pkg := range []string{api, web, dep} {
		pkgSpan := findSpan(spans, pkg)
		require.NotNil(t, pkgSpan, pkg)
		assert.Equal(t, codes.Error, pkgSpan.Status().Code, pkg)
		assert.Equal(t, compileErrors+"FAIL\t"+pkg+" [build failed]\n", logsOn(logs.Records(), pkgSpan), pkg)
	}
	cartSpan := findSpan(spans, cart)
	require.NotNil(t, cartSpan)
	assert.Equal(t, "ok  \t"+cart+"\t0.010s\n", logsOn(logs.Records(), cartSpan))
	test := findSpanByTestCaseName(spans, "TestCart")
	require.NotNil(t, test)
	assert.Empty(t, logsOn(logs.Records(), test))

	// go test prints the dependency's errors once.
	assert.Equal(t, 1, strings.Count(out, compileErrors), out)
}

// go test names a package's setup failure, such as a missing import, by the
// ImportPath of the missing package, and reuses it for every package that
// imports it; each package span logs only its own errors.
func TestSetupFailuresSharingImportPathAreLoggedApart(t *testing.T) {
	t.Parallel()

	now := time.Now()
	missing := "example.com/shop/missing"
	setupErrors := func(pkg string) string {
		return "# " + pkg + "\n" +
			strings.TrimPrefix(pkg, "example.com/shop/") + "/x_test.go:5:2: no required module provides package " + missing + "; to add it:\n" +
			"\tgo get " + missing + "\n"
	}
	var events []gotest.TestEvent
	for _, pkg := range []string{"example.com/shop/c1", "example.com/shop/c2"} {
		for line := range strings.Lines(setupErrors(pkg)) {
			events = append(events, buildEvent(now, missing, "build-output", line))
		}
		events = append(events,
			buildEvent(now, missing, "build-fail", ""),
			gotest.TestEvent{Time: now, Action: "start", Package: pkg},
			gotest.TestEvent{Time: now, Action: "output", Package: pkg, Output: "FAIL\t" + pkg + " [setup failed]\n"},
			gotest.TestEvent{Time: now, Action: "fail", Package: pkg, FailedBuild: missing},
		)
	}
	spans, _, logs := runWithOutput(t, events)

	for _, pkg := range []string{"example.com/shop/c1", "example.com/shop/c2"} {
		pkgSpan := findSpan(spans, pkg)
		require.NotNil(t, pkgSpan, pkg)
		assert.Equal(t,
			setupErrors(pkg)+"FAIL\t"+pkg+" [setup failed]\n",
			logsOn(logs.Records(), pkgSpan), pkg)
	}
}

// A package that fails setup for several reasons, such as two missing
// imports, gets each reported under its own ImportPath, though its fail
// event names only the first; its span logs them all.
func TestSetupFailureWithSeveralErrorsIsLoggedWhole(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pkg := "example.com/shop/c1"
	var events []gotest.TestEvent
	var setupErrors string
	for _, imp := range []struct{ missing, pos string }{
		{"example.com/shop/missing1", "c1/c1.go:4:2"},
		{"example.com/shop/missing2", "c1/c1.go:5:2"},
	} {
		errs := "# " + pkg + "\n" +
			imp.pos + ": no required module provides package " + imp.missing + "; to add it:\n" +
			"\tgo get " + imp.missing + "\n"
		setupErrors += errs
		for line := range strings.Lines(errs) {
			events = append(events, buildEvent(now, imp.missing, "build-output", line))
		}
		events = append(events, buildEvent(now, imp.missing, "build-fail", ""))
	}
	events = append(events,
		gotest.TestEvent{Time: now, Action: "start", Package: pkg},
		gotest.TestEvent{Time: now, Action: "output", Package: pkg, Output: "FAIL\t" + pkg + " [setup failed]\n"},
		gotest.TestEvent{Time: now, Action: "fail", Package: pkg, FailedBuild: "example.com/shop/missing1"},
	)
	spans, _, logs := runWithOutput(t, events)

	pkgSpan := findSpan(spans, pkg)
	require.NotNil(t, pkgSpan)
	assert.Equal(t, setupErrors+"FAIL\t"+pkg+" [setup failed]\n", logsOn(logs.Records(), pkgSpan))
}

func TestPassingPackageWithTests(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pkg := "example.com/pkg"
	spans := runEvents(t, []gotest.TestEvent{
		{Time: now, Action: "start", Package: pkg},
		{Time: now, Action: "run", Package: pkg, Test: "TestOK"},
		{Time: now, Action: "pass", Package: pkg, Test: "TestOK"},
		{Time: now, Action: "pass", Package: pkg},
	})

	pkgSpan := findSpan(spans, pkg)
	require.NotNil(t, pkgSpan)
	assert.Equal(t, codes.Ok, pkgSpan.Status().Code)
	assert.Contains(t, pkgSpan.Attributes(), semconv.TestCaseResultStatusPass)
	assert.NotContains(t, pkgSpan.Attributes(), semconv.TestSuiteRunStatusSkipped)
}

func TestSpanCount(t *testing.T) {
	spans := runFixture(t)

	spanNames := make([]string, len(spans))
	for i := range spans {
		spanNames[i] = spans[i].Name()
	}
	// 1 suite + 9 tests = 10 spans
	assert.ElementsMatch(t, spanNames, []string{
		"TestPass",
		"TestFail",
		"TestSkip",
		"level2",
		"level1",
		"TestSub",
		"TestParallel0",
		"TestParallel1",
		"TestParallel2",
		"a",
		"b",
		"continue",
		"continue",
		"continue",
		"continue",
		"continue",
		"github.com/dagger/otel-go/gotest/testdata/sample",
	})
}
