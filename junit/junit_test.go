package junit_test

import (
	"os"
	"testing"
	"time"

	"github.com/dagger/otel-go/junit"
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

	f, err := os.Open("testdata/sample.xml")
	require.NoError(t, err)
	defer f.Close()

	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	err = junit.Run(t.Context(), f, tp)
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
	assert.Contains(t, span.Attributes(), semconv.TestCaseResultStatusPass)
	assert.Equal(t, "TestPass",
		spanAttr(span, semconv.TestCaseNameKey).AsString())
	assert.True(t, span.EndTime().After(span.StartTime()))
}

func TestFailingTest(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestFail")
	require.NotNil(t, span, "expected span for TestFail")

	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Contains(t, span.Attributes(), semconv.TestSuiteRunStatusFailure)
}

func TestSkippedTest(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestSkip")
	require.NotNil(t, span, "expected span for TestSkip")

	assert.Equal(t, codes.Ok, span.Status().Code)
	assert.Contains(t, span.Attributes(), semconv.TestSuiteRunStatusSkipped)
}

func TestSuiteSpan(t *testing.T) {
	spans := runFixture(t)

	suite := findSpan(spans, "github.com/dagger/otel-go/gotest/testdata/sample")
	require.NotNil(t, suite, "expected suite span")

	// Suite has failures so it should be marked as error.
	assert.Equal(t, codes.Error, suite.Status().Code)
	assert.Contains(t, suite.Attributes(), semconv.TestSuiteRunStatusFailure)

	// All test spans should be children of the suite.
	pass := findSpan(spans, "TestPass")
	require.NotNil(t, pass)
	assert.Equal(t, suite.SpanContext().SpanID(), pass.Parent().SpanID())
}

func TestSubtestSpanName(t *testing.T) {
	spans := runFixture(t)

	// JUnit flattens subtests as "TestSub/level1/level2".
	// Span name should be the raw test name, not simplified.
	span := findSpan(spans, "TestSub/level1/level2")
	require.NotNil(t, span, "expected span with full name 'TestSub/level1/level2'")

	assert.Equal(t, "TestSub/level1/level2",
		spanAttr(span, semconv.TestCaseNameKey).AsString())
}

func TestTimestamp(t *testing.T) {
	spans := runFixture(t)

	// The sample XML has timestamp="2026-04-29T19:19:07-04:00" on the suite.
	expectedStart, err := time.Parse(time.RFC3339, "2026-04-29T19:19:07-04:00")
	require.NoError(t, err)

	suite := findSpan(spans, "github.com/dagger/otel-go/gotest/testdata/sample")
	require.NotNil(t, suite)
	assert.Equal(t, expectedStart, suite.StartTime(), "suite should start at XML timestamp")

	// Test spans should also start at the suite timestamp.
	pass := findSpan(spans, "TestPass")
	require.NotNil(t, pass)
	assert.Equal(t, expectedStart, pass.StartTime(), "test should start at suite timestamp")

	// End time = start + duration (TestPass has time="0.010").
	assert.Equal(t, expectedStart.Add(10*time.Millisecond), pass.EndTime(),
		"test end time should be start + duration")
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
		"TestSub",
		"TestSub/level1",
		"TestSub/level1/level2",
		"TestParallel0",
		"TestParallel1",
		"TestParallel2",
		"TestParallel1/a",
		"TestParallel1/b",
		"github.com/dagger/otel-go/gotest/testdata/sample",
	})
}
