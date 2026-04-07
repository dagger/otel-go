// Package gotest reads a go test -json stream and emits OTel spans
// for each test, with proper parent/child nesting for subtests.
package gotest

//go:generate sh -c "go -C testdata/sample test -json -count=1 ./... > testdata/sample.jsonl 2>&1 || true"

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	otel "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationLibrary = "dagger.io/otelgotest"

// TestEvent is the JSON structure emitted by go test -json.
// See: go doc cmd/test2json
type TestEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
	Output  string    `json:"Output"`
}

// testSpan tracks an in-flight span for a single test.
type testSpan struct {
	span    trace.Span
	ctx     context.Context
	output  strings.Builder
	streams *otel.SpanStreams
}

// Option configures the behavior of Run.
type Option func(*runConfig)

type runConfig struct {
	output         io.Writer
	loggerProvider *sdklog.LoggerProvider
}

// WithOutput passes through the human-readable test output (the Output
// field of each JSON event) to w. This reconstructs what go test would
// normally print, regardless of whether the caller is consuming JSON.
func WithOutput(w io.Writer) Option {
	return func(c *runConfig) { c.output = w }
}

// WithLoggerProvider routes test output to each test's span as OTel log
// records via [otel.SpanStdio]. Without this, output is only captured
// as span events.
func WithLoggerProvider(lp *sdklog.LoggerProvider) Option {
	return func(c *runConfig) { c.loggerProvider = lp }
}

// Run reads a go test -json stream and emits OTel spans in real time.
func Run(ctx context.Context, r io.Reader, tp trace.TracerProvider, opts ...Option) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	tracer := tp.Tracer(instrumentationLibrary)

	// key: "package/TestName" or "package/TestName/sub"
	spans := map[string]*testSpan{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var ev TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return fmt.Errorf("decoding test event: %w", err)
		}

		// Pass through the human-readable output.
		if cfg.output != nil && ev.Output != "" {
			io.WriteString(cfg.output, ev.Output)
		}

		// Skip package-level events (no test name).
		if ev.Test == "" {
			continue
		}

		key := ev.Package + "/" + ev.Test

		switch ev.Action {
		case "run":
			parentCtx := ctx
			// For subtests, find the parent span.
			if idx := strings.LastIndex(ev.Test, "/"); idx != -1 {
				parentKey := ev.Package + "/" + ev.Test[:idx]
				if ps, ok := spans[parentKey]; ok {
					parentCtx = ps.ctx
				}
			}

			// Span name is the base name (leaf).
			spanName := ev.Test
			if idx := strings.LastIndex(ev.Test, "/"); idx != -1 {
				spanName = ev.Test[idx+1:]
			}

			spanCtx, span := tracer.Start(parentCtx, spanName,
				trace.WithTimestamp(ev.Time),
				trace.WithAttributes(
					semconv.TestCaseName(ev.Test),
					semconv.TestSuiteName(ev.Package),
				),
			)

			ts := &testSpan{
				span: span,
				ctx:  spanCtx,
			}
			if cfg.loggerProvider != nil {
				spanCtx = otel.WithLoggerProvider(spanCtx, cfg.loggerProvider)
				streams := otel.SpanStdio(spanCtx, instrumentationLibrary)
				ts.streams = &streams
			}
			spans[key] = ts

		case "output":
			if ts, ok := spans[key]; ok {
				// Filter out the === RUN / --- PASS/FAIL/SKIP lines.
				trimmed := strings.TrimSpace(ev.Output)
				if trimmed == "" ||
					strings.HasPrefix(trimmed, "=== RUN") ||
					strings.HasPrefix(trimmed, "=== PAUSE") ||
					strings.HasPrefix(trimmed, "=== CONT") ||
					strings.HasPrefix(trimmed, "--- PASS") ||
					strings.HasPrefix(trimmed, "--- FAIL") ||
					strings.HasPrefix(trimmed, "--- SKIP") {
					continue
				}

				ts.output.WriteString(ev.Output)
				ts.span.AddEvent(trimmed, trace.WithTimestamp(ev.Time))

				// Route to span logs if configured.
				if ts.streams != nil {
					io.WriteString(ts.streams.Stdout, ev.Output)
				}
			}

		case "pass":
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				ts.span.SetStatus(codes.Ok, "test passed")
				ts.span.SetAttributes(semconv.TestCaseResultStatusPass)
				ts.span.End(trace.WithTimestamp(ev.Time))
				delete(spans, key)
			}

		case "fail":
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				desc := extractErrorOutput(ts.output.String())
				if desc == "" {
					desc = "test failed"
				}
				ts.span.SetStatus(codes.Error, desc)
				ts.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
				ts.span.End(trace.WithTimestamp(ev.Time))
				delete(spans, key)
			}

		case "skip":
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				ts.span.SetStatus(codes.Ok, "test skipped")
				ts.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
				ts.span.End(trace.WithTimestamp(ev.Time))
				delete(spans, key)
			}
		}
	}

	return scanner.Err()
}

// extractErrorOutput cleans up test output to use as an error description.
// It strips the file:line prefix that Go's testing package adds.
func extractErrorOutput(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Strip "    sample_test.go:15: " prefix.
		if idx := strings.Index(trimmed, ": "); idx != -1 {
			prefix := trimmed[:idx]
			if strings.Contains(prefix, ".go:") {
				trimmed = trimmed[idx+2:]
			}
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}
