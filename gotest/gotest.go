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
	registry       *SpanContextRegistry
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

// WithSpanContextRegistry configures a [SpanContextRegistry] that receives
// the span context of every test span created by [Run]. This allows an
// external coordinator (e.g., a Unix socket server) to serve span contexts
// to the test binary for cross-process context propagation.
func WithSpanContextRegistry(r *SpanContextRegistry) Option {
	return func(c *runConfig) { c.registry = r }
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

	// pkgSpans tracks a parent span per package.
	pkgSpans := map[string]*testSpan{}

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

		// Handle package-level events (no test name).
		if ev.Test == "" {
			switch ev.Action {
			case "start":
				pkgCtx, pkgSpan := tracer.Start(ctx, ev.Package,
					trace.WithTimestamp(ev.Time),
					trace.WithAttributes(
						semconv.TestSuiteName(ev.Package),
					),
				)
				pkgSpans[ev.Package] = &testSpan{
					span: pkgSpan,
					ctx:  pkgCtx,
				}
			case "pass":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Ok, "")
					ps.span.SetAttributes(semconv.TestCaseResultStatusPass)
					ps.span.End(trace.WithTimestamp(ev.Time))
					delete(pkgSpans, ev.Package)
				}
			case "fail":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Error, "package had failures")
					ps.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
					ps.span.End(trace.WithTimestamp(ev.Time))
					delete(pkgSpans, ev.Package)
				}
			}
			continue
		}

		key := ev.Package + "/" + ev.Test

		switch ev.Action {
		case "run":
			// Default parent is the package span, or the top-level ctx.
			parentCtx := ctx
			if ps, ok := pkgSpans[ev.Package]; ok {
				parentCtx = ps.ctx
			}
			// For subtests, find the parent test span.
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

			if cfg.registry != nil {
				cfg.registry.Register(ev.Test, span.SpanContext())
			}

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
// It strips file:line prefixes from Go's testing package and extracts the
// meaningful content from verbose testify-style error messages (keeping
// only "Error:" and "Messages:" sections).
func extractErrorOutput(output string) string {
	// First try to extract testify-style structured errors.
	if cleaned := cleanTestifyMessage(output); cleaned != "" {
		return cleaned
	}

	// Fall back to simple line-by-line cleanup.
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

// cleanTestifyMessage extracts the meaningful content from verbose
// testify-style error messages. It keeps only "Error:" and "Messages:"
// sections, stripping "Error Trace:" and "Test:" sections.
func cleanTestifyMessage(msg string) string {
	if !strings.Contains(msg, "\tError:") {
		return ""
	}

	lines := strings.Split(msg, "\n")
	var result []string
	inWanted := false
	found := false

	for _, line := range lines {
		// Section headers look like: \t<Name>:<padding>\t<value>
		if len(line) > 1 && line[0] == '\t' && line[1] != ' ' && line[1] != '\t' {
			inWanted = false
			rest := line[1:]
			colonIdx := strings.Index(rest, ":")
			if colonIdx < 0 {
				continue
			}
			name := rest[:colonIdx]
			after := strings.TrimLeft(rest[colonIdx+1:], " ")
			if len(after) == 0 || after[0] != '\t' {
				continue
			}
			if name == "Error" || name == "Messages" {
				inWanted = true
				found = true
				if v := strings.TrimSpace(after[1:]); v != "" {
					result = append(result, v)
				}
			}
			continue
		}

		// Continuation lines look like: \t<spaces>\t<value>
		if inWanted && len(line) > 0 && line[0] == '\t' {
			rest := strings.TrimLeft(line[1:], " ")
			if len(rest) > 0 && rest[0] == '\t' {
				if v := strings.TrimSpace(rest[1:]); v != "" {
					result = append(result, v)
				}
			}
		}
	}

	if found && len(result) > 0 {
		return strings.Join(result, "\n")
	}
	return ""
}
