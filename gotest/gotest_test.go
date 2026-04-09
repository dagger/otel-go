package gotest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dagger/otel-go/gotest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

func runEvents(t *testing.T, events []gotest.TestEvent) []sdktrace.ReadOnlySpan {
	t.Helper()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		require.NoError(t, enc.Encode(ev))
	}

	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	err := gotest.Run(t.Context(), &buf, tp)
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

	parent := findSpan(spans, "TestParallel")
	require.NotNil(t, parent, "expected span for TestParallel")

	a := findSpan(spans, "a")
	require.NotNil(t, a, "expected span for parallel/a")

	b := findSpan(spans, "b")
	require.NotNil(t, b, "expected span for parallel/b")

	// Both should be children of TestParallel
	assert.Equal(t, parent.SpanContext().SpanID(), a.Parent().SpanID())
	assert.Equal(t, parent.SpanContext().SpanID(), b.Parent().SpanID())

	// Both should pass
	assert.Equal(t, codes.Ok, a.Status().Code)
	assert.Equal(t, codes.Ok, b.Status().Code)
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
	// TestParallel, TestParallel/a, TestParallel/b
	// = 10 total
	assert.Len(t, spans, 10)
}
