package gotest_test

import (
	"os"
	"slices"
	"strings"
	"testing"

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

func TestOutputCaptured(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestPass")
	require.NotNil(t, span)

	events := span.Events()
	found := slices.ContainsFunc(events, func(e sdktrace.Event) bool {
		return strings.Contains(e.Name, "this test passes")
	})
	assert.True(t, found, "expected output event containing 'this test passes', got events: %v", events)
}

func TestSpanCount(t *testing.T) {
	spans := runFixture(t)

	// TestPass, TestFail, TestSkip,
	// TestSub, TestSub/level1, TestSub/level1/level2,
	// TestParallel, TestParallel/a, TestParallel/b
	// = 9 total
	assert.Len(t, spans, 9)
}
