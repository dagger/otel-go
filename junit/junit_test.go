package junit_test

import (
	"os"
	"slices"
	"strings"
	"testing"

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
	assert.Contains(t, span.Status().Description, "something went wrong")
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

	suite := findSpan(spans, "github.com/example/pkg")
	require.NotNil(t, suite, "expected suite span")

	// Suite has failures, so it should be marked as error.
	assert.Equal(t, codes.Error, suite.Status().Code)
	assert.Contains(t, suite.Attributes(), semconv.TestSuiteRunStatusFailure)

	// All test spans should be children of the suite.
	pass := findSpan(spans, "TestPass")
	require.NotNil(t, pass)
	assert.Equal(t, suite.SpanContext().SpanID(), pass.Parent().SpanID())
}

func TestOutputCaptured(t *testing.T) {
	spans := runFixture(t)

	span := findSpan(spans, "TestPass")
	require.NotNil(t, span)

	events := span.Events()
	found := slices.ContainsFunc(events, func(e sdktrace.Event) bool {
		return strings.Contains(e.Name, "this test passes")
	})
	assert.True(t, found, "expected output event containing 'this test passes'")
}

func TestSpanCount(t *testing.T) {
	spans := runFixture(t)

	// 1 suite + 5 tests = 6 spans
	assert.Len(t, spans, 6)
}
