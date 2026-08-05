// Package junit converts JUnit XML test suites into OTel spans.
package junit

import (
	"context"
	"io"
	"time"

	otel "github.com/dagger/otel-go"
	junitparser "github.com/joshdk/go-junit"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationLibrary = "dagger.io/oteljunit"

// Option configures the behavior of Run.
type Option func(*runConfig)

type runConfig struct {
	loggerProvider *sdklog.LoggerProvider
}

// WithLoggerProvider routes test output to each test's span as OTel log
// records via [otel.SpanStdio].
func WithLoggerProvider(lp *sdklog.LoggerProvider) Option {
	return func(c *runConfig) { c.loggerProvider = lp }
}

// Run parses JUnit XML from r and emits OTel spans for each test.
func Run(ctx context.Context, r io.Reader, tp trace.TracerProvider, opts ...Option) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	suites, err := junitparser.IngestReader(r)
	if err != nil {
		return err
	}

	tracer := tp.Tracer(instrumentationLibrary)

	for _, suite := range suites {
		emitSuite(ctx, tracer, &cfg, suite)
	}

	return nil
}

// suiteTimestamp parses the JUnit timestamp attribute from a suite's properties.
func suiteTimestamp(suite junitparser.Suite) time.Time {
	ts, ok := suite.Properties["timestamp"]
	if !ok {
		return time.Time{}
	}
	// Try RFC3339 first (includes timezone), then the common JUnit format.
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

func emitSuite(ctx context.Context, tracer trace.Tracer, cfg *runConfig, suite junitparser.Suite) {
	suiteName := suite.Name
	if suiteName == "" {
		suiteName = suite.Package
	}
	if suiteName == "" {
		suiteName = "suite"
	}

	ts := suiteTimestamp(suite)

	var startOpts []trace.SpanStartOption
	startOpts = append(startOpts, trace.WithAttributes(
		semconv.TestSuiteName(suiteName),
	))
	if !ts.IsZero() {
		startOpts = append(startOpts, trace.WithTimestamp(ts))
	}

	suiteCtx, suiteSpan := tracer.Start(ctx, suiteName, startOpts...)

	for _, test := range suite.Tests {
		emitTest(suiteCtx, tracer, cfg, suiteName, ts, test)
	}

	// Nested suites.
	for _, child := range suite.Suites {
		emitSuite(suiteCtx, tracer, cfg, child)
	}

	// Set suite status based on totals.
	if suite.Totals.Failed > 0 || suite.Totals.Error > 0 {
		suiteSpan.SetStatus(codes.Error, "suite had failures")
		suiteSpan.SetAttributes(semconv.TestSuiteRunStatusFailure)
	} else {
		suiteSpan.SetStatus(codes.Ok, "")
		suiteSpan.SetAttributes(semconv.TestCaseResultStatusPass)
	}

	var endOpts []trace.SpanEndOption
	if !ts.IsZero() {
		endOpts = append(endOpts, trace.WithTimestamp(ts.Add(suite.Totals.Duration)))
	}
	suiteSpan.End(endOpts...)
}

func emitTest(ctx context.Context, tracer trace.Tracer, cfg *runConfig, suiteName string, suiteStart time.Time, test junitparser.Test) {
	if test.Duration == 0 {
		// avoid gotcha; end time has to be .After(startTime)
		test.Duration = 1
	}
	var startTime, endTime time.Time
	if !suiteStart.IsZero() {
		startTime = suiteStart
		endTime = suiteStart.Add(test.Duration)
	} else {
		endTime = time.Now()
		startTime = endTime.Add(-test.Duration)
	}

	spanCtx, span := tracer.Start(ctx, test.Name,
		trace.WithTimestamp(startTime),
		trace.WithAttributes(
			semconv.TestCaseName(test.Name),
			semconv.TestSuiteName(suiteName),
		),
	)

	// Emit output as span logs if configured.
	if cfg.loggerProvider != nil && (test.SystemOut != "" || test.SystemErr != "") {
		logCtx := otel.WithLoggerProvider(spanCtx, cfg.loggerProvider)
		streams := otel.SpanStdio(logCtx, instrumentationLibrary)
		if test.SystemOut != "" {
			_, _ = io.WriteString(streams.Stdout, test.SystemOut)
		}
		if test.SystemErr != "" {
			_, _ = io.WriteString(streams.Stderr, test.SystemErr)
		}
		_ = streams.Close()
	}

	switch test.Status {
	case junitparser.StatusPassed:
		span.SetStatus(codes.Ok, "")
		span.SetAttributes(semconv.TestCaseResultStatusPass)

	case junitparser.StatusFailed, junitparser.StatusError:
		desc := test.Message
		if desc == "" && test.Error != nil {
			desc = test.Error.Error()
		}
		if desc == "" {
			desc = "test failed"
		}
		span.SetStatus(codes.Error, desc)
		span.SetAttributes(semconv.TestSuiteRunStatusFailure)

	case junitparser.StatusSkipped:
		span.SetStatus(codes.Ok, "")
		span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
	}

	span.End(trace.WithTimestamp(endTime))
}
