// Package gotest reads a go test -json stream and emits OTel spans
// for each test, with proper parent/child nesting for subtests.
package gotest

//go:generate sh -c "go -C testdata/sample test -json -count=1 ./... > testdata/sample.jsonl 2>&1 || true"

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	dagotel "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel/attribute"
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
	span      trace.Span
	spanStart time.Time
	// ctx is the original test span context used to parent subtests. For
	// parallel tests, it intentionally remains the pre-PAUSE span even after
	// ts.span is replaced by the internal continuation span.
	ctx         context.Context
	parentCtx   context.Context
	testName    string
	spanName    string
	output      strings.Builder
	streams     *dagotel.SpanStreams
	bufferedOut strings.Builder // buffered output for non-verbose mode

	// When a test calls t.Parallel(), go test emits a pause event while the test
	// waits to be scheduled. End the current span at pause time and remember its
	// context so the continuation span can link back to it when cont is emitted.
	pausedSpanContext trace.SpanContext
	paused            bool
}

// Option configures the behavior of Run.
type Option func(*runConfig)

type runConfig struct {
	output         io.Writer
	verbose        bool
	loggerProvider *sdklog.LoggerProvider
	registry       *SpanContextRegistry
}

// WithOutput passes through the human-readable test output (the Output
// field of each JSON event) to w. This reconstructs what go test would
// normally print, regardless of whether the caller is consuming JSON.
//
// By default the output mimics non-verbose go test: per-test output is
// only shown for failing tests. Use [WithVerbose] to show all output.
func WithOutput(w io.Writer) Option {
	return func(c *runConfig) { c.output = w }
}

// WithVerbose controls whether the human-readable output written via
// [WithOutput] includes per-test detail for passing tests. When false
// (the default), output is only shown for failing tests, matching the
// behavior of go test without -v.
//
// Note: go test -json always forces -test.v=test2json internally,
// which makes testing.Verbose() return true inside test binaries.
// This option only affects the reconstructed human-readable output;
// it cannot change the behavior of testing.Verbose().
func WithVerbose(v bool) Option {
	return func(c *runConfig) { c.verbose = v }
}

// WithLoggerProvider routes test output to each test's span as OTel log
// records via [dagotel.SpanStdio]. Without this, output is only captured
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

func elapsedDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func spanEndTime(ts *testSpan, ev TestEvent) time.Time {
	if ts != nil && !ts.spanStart.IsZero() && ev.Elapsed > 0 {
		return ts.spanStart.Add(elapsedDuration(ev.Elapsed))
	}
	return ev.Time
}

// Run reads a go test -json stream and emits OTel spans in real time.
func Run(ctx context.Context, r io.Reader, tp trace.TracerProvider, opts ...Option) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	tracer := tp.Tracer(instrumentationLibrary)

	// key: package-qualified full test name (package + "/" + ev.Test).
	spans := map[string]*testSpan{}

	// activeTests tracks currently running tests per package. A test is active
	// after "run" or "cont", inactive after "pause", and removed after
	// "pass"/"fail"/"skip". Parentage is derived from the longest active
	// test name prefix instead of splitting ev.Test on '/'; this preserves leaf
	// names that themselves contain '/'.
	activeTests := map[string]map[string]struct{}{}

	// pkgSpans tracks a parent span per package.
	pkgSpans := map[string]*testSpan{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var ev TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			// Non-JSON line (e.g., build error). Pass through to output.
			if cfg.output != nil {
				io.WriteString(cfg.output, scanner.Text()+"\n")
			}
			continue
		}

		// Pass through the human-readable output.
		// In verbose mode, write immediately. In non-verbose mode,
		// buffer per-test output and only flush it on failure.
		if cfg.output != nil && ev.Output != "" {
			if ev.Test != "" && !cfg.verbose {
				// Buffer test-specific output.
				key := ev.Package + "/" + ev.Test
				if ts, ok := spans[key]; ok {
					ts.bufferedOut.WriteString(ev.Output)
				}
			} else if cfg.verbose || strings.TrimSpace(ev.Output) != "PASS" {
				io.WriteString(cfg.output, ev.Output)
			}
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
					span:      pkgSpan,
					spanStart: ev.Time,
					ctx:       pkgCtx,
				}
			case "pass":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Ok, "")
					ps.span.SetAttributes(semconv.TestCaseResultStatusPass)
					ps.span.End(trace.WithTimestamp(spanEndTime(ps, ev)))
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
			case "fail":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Error, "package had failures")
					ps.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
					ps.span.End(trace.WithTimestamp(spanEndTime(ps, ev)))
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
			case "skip":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Ok, "skipped")
					ps.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
					ps.span.End(trace.WithTimestamp(spanEndTime(ps, ev)))
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
			}
			continue
		}

		key := ev.Package + "/" + ev.Test

		switch ev.Action {
		case "run":
			// Default parent is the package span, or the top-level ctx.
			parentCtx := ctx
			var parentName string
			if ps, ok := pkgSpans[ev.Package]; ok {
				parentCtx = ps.ctx
			}
			if active := activeTests[ev.Package]; active != nil {
				longest := -1
				for activeKey := range active {
					ts, ok := spans[activeKey]
					if !ok {
						continue
					}
					prefix := ts.testName + "/"
					if strings.HasPrefix(ev.Test, prefix) && len(ts.testName) > longest {
						parentCtx = ts.ctx
						parentName = ts.testName
						longest = len(ts.testName)
					}
				}
			}

			spanName := ev.Test
			if parentName != "" {
				spanName = strings.TrimPrefix(ev.Test, parentName+"/")
			}

			spanCtx, span := tracer.Start(parentCtx, spanName,
				trace.WithTimestamp(ev.Time),
				trace.WithAttributes(
					semconv.TestCaseName(ev.Test),
					semconv.TestSuiteName(ev.Package),
					attribute.Bool(dagotel.UIBoundaryAttr, true),
				),
			)

			ts := &testSpan{
				span:      span,
				spanStart: ev.Time,
				ctx:       spanCtx,
				parentCtx: parentCtx,
				testName:  ev.Test,
				spanName:  spanName,
			}
			if cfg.loggerProvider != nil {
				spanCtx = dagotel.WithLoggerProvider(spanCtx, cfg.loggerProvider)
				ts.ctx = spanCtx
				streams := dagotel.SpanStdio(spanCtx, instrumentationLibrary)
				ts.streams = &streams
			}
			spans[key] = ts
			if activeTests[ev.Package] == nil {
				activeTests[ev.Package] = map[string]struct{}{}
			}
			activeTests[ev.Package][key] = struct{}{}

			if cfg.registry != nil {
				cfg.registry.Register(key, span.SpanContext())
			}

		case "pause":
			if active := activeTests[ev.Package]; active != nil {
				delete(active, key)
			}
			if ts, ok := spans[key]; ok && !ts.paused {
				// Keep ts.streams open so all test output remains associated with
				// the original, user-facing span rather than the internal continuation.
				ts.pausedSpanContext = ts.span.SpanContext()
				ts.paused = true
				ts.span.End(trace.WithTimestamp(ev.Time))
			}

		case "cont":
			if ts, ok := spans[key]; ok {
				if ts.paused {
					linkSC := ts.pausedSpanContext
					if !linkSC.IsValid() {
						linkSC = ts.span.SpanContext()
					}

					_, span := tracer.Start(ts.parentCtx, ts.spanName+" (continued)",
						trace.WithTimestamp(ev.Time),
						trace.WithAttributes(
							semconv.TestCaseName(ev.Test),
							semconv.TestSuiteName(ev.Package),
							attribute.Bool(dagotel.UIPassthroughAttr, true),
						),
						trace.WithLinks(trace.Link{
							SpanContext: linkSC,
							Attributes: []attribute.KeyValue{
								attribute.String(dagotel.LinkPurposeAttr, dagotel.LinkPurposeCause),
							},
						}),
					)
					ts.span = span
					ts.spanStart = ev.Time
					// Keep ts.ctx pointing at the original span so subtests remain
					// parented under the user-facing test span, not the internal
					// continuation span.
					ts.paused = false

					if cfg.registry != nil {
						cfg.registry.Register(key, span.SpanContext())
					}
				}

				if activeTests[ev.Package] == nil {
					activeTests[ev.Package] = map[string]struct{}{}
				}
				activeTests[ev.Package][key] = struct{}{}
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

				// Route to span logs if configured.
				if ts.streams != nil {
					io.WriteString(ts.streams.Stdout, ev.Output)
				}
			}

		case "pass":
			if active := activeTests[ev.Package]; active != nil {
				delete(active, key)
			}
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				// Non-verbose: discard buffered output for passing tests.
				ts.span.SetStatus(codes.Ok, "test passed")
				ts.span.SetAttributes(semconv.TestCaseResultStatusPass)
				ts.span.End(trace.WithTimestamp(spanEndTime(ts, ev)))
				delete(spans, key)
			}

		case "fail":
			if active := activeTests[ev.Package]; active != nil {
				delete(active, key)
			}
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				// Non-verbose: flush buffered output for failing tests.
				if cfg.output != nil && !cfg.verbose && ts.bufferedOut.Len() > 0 {
					io.WriteString(cfg.output, ts.bufferedOut.String())
				}
				desc := extractErrorOutput(ts.output.String())
				if desc == "" {
					desc = "test failed"
				}
				ts.span.SetStatus(codes.Error, desc)
				ts.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
				ts.span.End(trace.WithTimestamp(spanEndTime(ts, ev)))
				delete(spans, key)
			}

		case "skip":
			if active := activeTests[ev.Package]; active != nil {
				delete(active, key)
			}
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					ts.streams.Close()
				}
				// Non-verbose: discard buffered output for skipped tests.
				ts.span.SetStatus(codes.Ok, "test skipped")
				ts.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
				ts.span.End(trace.WithTimestamp(spanEndTime(ts, ev)))
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
//
// Note: Go's testing.decorate adds 4 spaces of indentation to
// continuation lines of multi-line messages, so lines may be
// prefixed with spaces before the tab characters.
func cleanTestifyMessage(msg string) string {
	if !strings.Contains(msg, "\tError:") {
		return ""
	}

	lines := strings.Split(msg, "\n")
	var result []string
	inWanted := false
	found := false

	for _, line := range lines {
		// Strip leading spaces added by Go's testing.decorate.
		line = strings.TrimLeft(line, " ")

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
