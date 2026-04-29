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
	defer f.Close()

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

func findPauseSpanFor(spans []sdktrace.ReadOnlySpan, testSpan sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() != "paused" {
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
	assert.Contains(t, span.Status().Description, "something went wrong")

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
	b := findSpanByTestCaseName(spans, "TestPar/B")
	require.NotNil(t, b)

	aChild := findSpanByTestCaseName(spans, "TestPar/A/child")
	require.NotNil(t, aChild)
	assert.Equal(t, "child", aChild.Name())
	assert.Equal(t, a.SpanContext().SpanID(), aChild.Parent().SpanID())

	bChild := findSpanByTestCaseName(spans, "TestPar/B/child")
	require.NotNil(t, bChild)
	assert.Equal(t, "child", bChild.Name())
	assert.Equal(t, b.SpanContext().SpanID(), bChild.Parent().SpanID())
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

func TestPauseContCreatesPausedSpanAndLinks(t *testing.T) {
	t.Parallel()

	startAt := time.Now()
	runAt := startAt.Add(time.Second)
	pauseAt := startAt.Add(2 * time.Second)
	contAt := startAt.Add(10 * time.Second)
	endAt := runAt.Add(2 * time.Second)
	passEventAt := startAt.Add(20 * time.Second)

	spans := runEvents(t, []gotest.TestEvent{
		{Time: startAt, Action: "start", Package: "example.com/pkg"},
		{Time: runAt, Action: "run", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: pauseAt, Action: "pause", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: contAt, Action: "cont", Package: "example.com/pkg", Test: "TestParallel"},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Test: "TestParallel", Elapsed: 2},
		{Time: passEventAt, Action: "pass", Package: "example.com/pkg", Elapsed: 20},
	})

	pkg := findSpan(spans, "example.com/pkg")
	require.NotNil(t, pkg)

	testSpans := findSpansByTestCaseName(spans, "TestParallel")
	require.Len(t, testSpans, 1)
	testSpan := testSpans[0]

	assert.True(t, testSpan.StartTime().Equal(runAt))
	assert.True(t, testSpan.EndTime().Equal(endAt))
	assert.Equal(t, codes.Ok, testSpan.Status().Code)
	assert.Equal(t, pkg.SpanContext().SpanID(), testSpan.Parent().SpanID())

	pauseSpan := findPauseSpanFor(spans, testSpan)
	require.NotNil(t, pauseSpan)
	assert.Equal(t, "paused", pauseSpan.Name())
	assert.True(t, pauseSpan.StartTime().Equal(pauseAt))
	assert.True(t, pauseSpan.EndTime().Equal(contAt))
	assert.Equal(t, testSpan.SpanContext().SpanID(), pauseSpan.Parent().SpanID())
	assert.True(t, spanAttr(pauseSpan, attribute.Key(otel.UIInternalAttr)).AsBool())
	assert.True(t, spanAttr(pauseSpan, attribute.Key(otel.UIPassthroughAttr)).AsBool())

	require.Len(t, pauseSpan.Links(), 1)
	assert.Equal(t, testSpan.SpanContext().TraceID(), pauseSpan.Links()[0].SpanContext.TraceID())
	assert.Equal(t, testSpan.SpanContext().SpanID(), pauseSpan.Links()[0].SpanContext.SpanID())
	assert.Contains(t, pauseSpan.Links()[0].Attributes,
		attribute.String(otel.LinkPurposeAttr, otel.LinkPurposePaused))
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
	testSpan := testSpans[0]
	pauseSpan := findPauseSpanFor(spans, testSpan)
	require.NotNil(t, pauseSpan)

	var outputLog *sdklog.Record
	for _, rec := range logExporter.Records() {
		if rec.Body().AsString() == "continued output\n" {
			rec := rec
			outputLog = &rec
			break
		}
	}
	require.NotNil(t, outputLog, "expected continued output log record")
	assert.Equal(t, testSpan.SpanContext().TraceID(), outputLog.TraceID())
	assert.Equal(t, testSpan.SpanContext().SpanID(), outputLog.SpanID())
	assert.NotEqual(t, pauseSpan.SpanContext().SpanID(), outputLog.SpanID(),
		"continued output should stay routed to the test span, not the pause span")
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

	pkg := findSpan(spans, "github.com/dagger/otel-go/gotest/testdata/sample")
	require.NotNil(t, pkg, "expected package span")

	p0 := findSpanByTestCaseName(spans, "TestParallel0")
	require.NotNil(t, p0, "expected span for TestParallel0")
	p1 := findSpanByTestCaseName(spans, "TestParallel1")
	require.NotNil(t, p1, "expected span for TestParallel1")
	p2 := findSpanByTestCaseName(spans, "TestParallel2")
	require.NotNil(t, p2, "expected span for TestParallel2")

	for _, span := range []sdktrace.ReadOnlySpan{p0, p1, p2} {
		assert.Equal(t, pkg.SpanContext().SpanID(), span.Parent().SpanID())
		assert.Equal(t, codes.Ok, span.Status().Code)
	}

	aSpans := findSpansByTestCaseName(spans, "TestParallel1/a")
	require.Len(t, aSpans, 1, "parallel/a should have one test span")
	a := aSpans[0]

	bSpans := findSpansByTestCaseName(spans, "TestParallel1/b")
	require.Len(t, bSpans, 1, "parallel/b should have one test span")
	b := bSpans[0]

	// Both subtests should be children of TestParallel1.
	assert.Equal(t, p1.SpanContext().SpanID(), a.Parent().SpanID())
	assert.Equal(t, p1.SpanContext().SpanID(), b.Parent().SpanID())

	// Both should pass.
	assert.Equal(t, codes.Ok, a.Status().Code)
	assert.Equal(t, codes.Ok, b.Status().Code)

	// Separate pause spans represent the time spent waiting for t.Parallel.
	assertPauseSpan := func(testSpan sdktrace.ReadOnlySpan) {
		t.Helper()

		pauseSpan := findPauseSpanFor(spans, testSpan)
		require.NotNil(t, pauseSpan)
		assert.Equal(t, "paused", pauseSpan.Name())
		assert.Equal(t, testSpan.SpanContext().SpanID(), pauseSpan.Parent().SpanID())
		assert.True(t, spanAttr(pauseSpan, attribute.Key(otel.UIInternalAttr)).AsBool())
		assert.True(t, spanAttr(pauseSpan, attribute.Key(otel.UIPassthroughAttr)).AsBool())
		require.Len(t, pauseSpan.Links(), 1)
		assert.Equal(t, testSpan.SpanContext().SpanID(), pauseSpan.Links()[0].SpanContext.SpanID())
		assert.Contains(t, pauseSpan.Links()[0].Attributes,
			attribute.String(otel.LinkPurposeAttr, otel.LinkPurposePaused))
		assert.False(t, pauseSpan.EndTime().Before(pauseSpan.StartTime()))
	}

	assertPauseSpan(p0)
	assertPauseSpan(p1)
	assertPauseSpan(p2)
	assertPauseSpan(a)
	assertPauseSpan(b)
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

func TestSpanCount(t *testing.T) {
	spans := runFixture(t)

	// 1 package +
	// TestPass, TestFail, TestSkip,
	// TestSub, TestSub/level1, TestSub/level1/level2,
	// TestParallel0/1/2 and their paused spans,
	// TestParallel1/a, TestParallel1/a paused,
	// TestParallel1/b, TestParallel1/b paused
	// = 17 total
	assert.Len(t, spans, 17)
}
