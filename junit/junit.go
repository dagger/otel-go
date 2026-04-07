// Package junit converts JUnit XML test suites into OTel spans.
package junit

import (
	"context"
	"io"
	"strings"
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

func emitSuite(ctx context.Context, tracer trace.Tracer, cfg *runConfig, suite junitparser.Suite) {
	suiteName := suite.Name
	if suiteName == "" {
		suiteName = suite.Package
	}
	if suiteName == "" {
		suiteName = "suite"
	}

	suiteCtx, suiteSpan := tracer.Start(ctx, suiteName,
		trace.WithAttributes(
			semconv.TestSuiteName(suiteName),
		),
	)

	for _, test := range suite.Tests {
		emitTest(suiteCtx, tracer, cfg, suiteName, test)
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

	suiteSpan.End()
}

func emitTest(ctx context.Context, tracer trace.Tracer, cfg *runConfig, suiteName string, test junitparser.Test) {
	// Use the base name for the span, full name for the attribute.
	spanName := test.Name
	if idx := strings.LastIndex(test.Name, "/"); idx != -1 {
		spanName = test.Name[idx+1:]
	}

	now := time.Now()
	startTime := now.Add(-test.Duration)

	spanCtx, span := tracer.Start(ctx, spanName,
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
			io.WriteString(streams.Stdout, test.SystemOut)
		}
		if test.SystemErr != "" {
			io.WriteString(streams.Stderr, test.SystemErr)
		}
		streams.Close()
	}

	// Emit output as span events too.
	if test.SystemOut != "" {
		span.AddEvent(strings.TrimSpace(test.SystemOut))
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

	span.End(trace.WithTimestamp(now))
}
