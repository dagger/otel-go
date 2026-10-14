// Package gotest reads a go test -json stream and emits OTel spans
// for each test, with proper parent/child nesting for subtests.
package gotest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
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
	packageName string
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

// timeoutPanicPrefix starts the output a test binary prints when go test's
// -timeout fires. Only this signature is matched: a passing test can print
// other panic text, such as a recovered panic, as ordinary output.
const timeoutPanicPrefix = "panic: test timed out after "

// timeoutStacksLimit bounds how much of the goroutine dump that follows a
// timeout panic is copied to the human-readable output when the package
// span's logs retain the whole dump. Everything before the first
// "goroutine " line, which holds the panic and its "running tests:" list,
// is copied whole, as are package summary lines such as the final FAIL.
// Without a logger provider nothing retains the dump, so it is not
// truncated.
const timeoutStacksLimit = 64 << 10

// timeoutDump follows a package's output for the test binary's timeout
// panic. test2json attributes that output to whichever test reported last,
// which may already have passed, and a timeout ends no running test, so
// per-test buffering would drop it; once found, it is routed to the package.
type timeoutDump struct {
	// head is the start of the current output line when it may still turn
	// out to be the signature; test2json can split a line across events.
	// headTest is the test the head's first fragment was attributed to.
	head     string
	headTest string
	// midLine is set when the current line can no longer be the signature.
	midLine bool

	active bool
	// retained is set when the package span's logs keep the whole dump,
	// which is what allows truncating the copy.
	retained bool

	// pending holds a partial line until it can be copied whole.
	pending   string
	inStacks  bool
	copied    int
	truncated bool
}

// scan reports where the timeout signature's line starts in text, attributed
// to test, and the part of that line which arrived in earlier events with
// the test that part was attributed to.
func (d *timeoutDump) scan(text, test string) (start int, carried, carriedTest string, found bool) {
	for i := 0; i < len(text); {
		seg := text[i:]
		nl := strings.IndexByte(seg, '\n')
		if nl >= 0 {
			seg = seg[:nl+1]
		}
		if !d.midLine {
			line := d.head + seg
			if strings.HasPrefix(line, timeoutPanicPrefix) {
				carried, carriedTest = d.head, d.headTest
				d.head, d.headTest = "", ""
				return i, carried, carriedTest, true
			}
			if nl < 0 && strings.HasPrefix(timeoutPanicPrefix, line) {
				if d.head == "" {
					d.headTest = test
				}
				d.head = line
				return 0, "", "", false
			}
		}
		d.head, d.headTest = "", ""
		d.midLine = nl < 0
		i += len(seg)
	}
	return 0, "", "", false
}

// copyTo copies dump output to w line by line: the panic line and the
// running-tests list whole, then the goroutine dump up to
// timeoutStacksLimit.
func (d *timeoutDump) copyTo(w io.Writer, text string) {
	d.pending += text
	for {
		nl := strings.IndexByte(d.pending, '\n')
		if nl < 0 {
			return
		}
		line := d.pending[:nl+1]
		d.pending = d.pending[nl+1:]
		d.copyLine(w, line)
	}
}

// flush copies a final partial line.
func (d *timeoutDump) flush(w io.Writer) {
	if d.pending != "" {
		line := d.pending
		d.pending = ""
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		d.copyLine(w, line)
	}
}

func (d *timeoutDump) copyLine(w io.Writer, line string) {
	if !d.inStacks && strings.HasPrefix(line, "goroutine ") {
		d.inStacks = true
	}
	if !d.inStacks || !d.retained || isPackageSummary(line) {
		_, _ = io.WriteString(w, line)
		return
	}
	if d.truncated {
		return
	}
	if d.copied+len(line) > timeoutStacksLimit {
		d.truncated = true
		_, _ = fmt.Fprintf(w, "... goroutine dump truncated after %d bytes; full dump in the package span logs\n", d.copied)
		return
	}
	d.copied += len(line)
	_, _ = io.WriteString(w, line)
}

// isPackageSummary reports whether line is one of go test's package result
// lines, which stay visible after a truncated dump.
func isPackageSummary(line string) bool {
	trimmed := strings.TrimRight(line, "\r\n")
	return trimmed == "FAIL" || trimmed == "PASS" ||
		strings.HasPrefix(trimmed, "FAIL\t") || strings.HasPrefix(trimmed, "ok  \t") ||
		strings.HasPrefix(trimmed, "exit status ")
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

// endPackageSpan ends a package span, and the log streams it may carry.
func endPackageSpan(ps *testSpan, ev TestEvent) {
	if ps.streams != nil {
		_ = ps.streams.Close()
	}
	ps.span.End(trace.WithTimestamp(spanEndTime(ps, ev)))
}

// finishUnfinishedTests finalizes the tests of a failed package that never
// reported a result, as when go test's -timeout panics the test binary: the
// running and paused tests get no event of their own. Their buffered output
// is printed, labeled, in test name order, and their spans end in error.
func finishUnfinishedTests(cfg *runConfig, spans map[string]*testSpan, ev TestEvent) {
	var keys []string
	for key, ts := range spans {
		if ts.packageName == ev.Package {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		ts := spans[key]
		// Verbose output was printed as it arrived.
		if cfg.output != nil && !cfg.verbose {
			_, _ = fmt.Fprintf(cfg.output, "--- UNFINISHED: %s (did not finish: package failed)\n", ts.testName)
			if ts.bufferedOut.Len() > 0 {
				out := ts.bufferedOut.String()
				if !strings.HasSuffix(out, "\n") {
					out += "\n"
				}
				_, _ = io.WriteString(cfg.output, out)
			}
		}
		if ts.streams != nil {
			_ = ts.streams.Close()
		}
		// A paused test's span already ended at its pause, and it never
		// continued.
		if !ts.paused {
			ts.span.SetStatus(codes.Error, "did not finish: package failed")
			ts.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
			ts.span.End(trace.WithTimestamp(ev.Time))
		}
		delete(spans, key)
	}
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

	// pkgRuns counts the test "run" events seen per package. When go test is
	// invoked with a -run pattern that matches nothing, the package still
	// reports "pass" (printing "[no tests to run]"), which would otherwise be
	// reported as a passing suite that ran nothing.
	pkgRuns := map[string]int{}

	// dumps follows each package's output for a test binary timeout panic.
	dumps := map[string]*timeoutDump{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var ev TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			// Non-JSON line (e.g., build error). Pass through to output.
			if cfg.output != nil {
				_, _ = io.WriteString(cfg.output, scanner.Text()+"\n")
			}
			continue
		}

		// From a timeout panic on, the package's output is the dump: route it
		// to the package span's logs and, bounded, to the human-readable
		// output instead of the test it is attributed to.
		passedThrough := false
		if ev.Action == "output" && ev.Output != "" {
			d := dumps[ev.Package]
			if d == nil {
				d = &timeoutDump{}
				dumps[ev.Package] = d
			}
			var dumpText, carried, carriedTest string
			if d.active {
				dumpText = ev.Output
			} else if start, head, headTest, found := d.scan(ev.Output, ev.Test); found {
				d.active = true
				dumpText, carried, carriedTest = ev.Output[start:], head, headTest
				if ps, ok := pkgSpans[ev.Package]; ok && ps.streams != nil {
					d.retained = true
				}
			}
			if dumpText != "" {
				if ps, ok := pkgSpans[ev.Package]; ok && ps.streams != nil {
					_, _ = io.WriteString(ps.streams.Stdout, carried+dumpText)
				}
				before := ev.Output[:len(ev.Output)-len(dumpText)]
				if cfg.output != nil {
					if cfg.verbose {
						// Verbose output is printed as it arrives, uncapped.
						_, _ = io.WriteString(cfg.output, ev.Output)
					} else {
						if ev.Test == "" && strings.TrimSpace(before) != "PASS" {
							_, _ = io.WriteString(cfg.output, before)
						}
						// A signature line split across events began in a
						// buffered test's output; move it to the dump, so it
						// is not printed again when that test is flushed.
						stdoutText := dumpText
						if carried != "" && carriedTest != "" {
							stdoutText = carried + dumpText
							if ts, ok := spans[ev.Package+"/"+carriedTest]; ok {
								if buffered := ts.bufferedOut.String(); strings.HasSuffix(buffered, carried) {
									ts.bufferedOut.Reset()
									ts.bufferedOut.WriteString(strings.TrimSuffix(buffered, carried))
								}
							}
						}
						d.copyTo(cfg.output, stdoutText)
					}
				}
				passedThrough = cfg.verbose
				ev.Output = before
				if before == "" || ev.Test == "" {
					continue
				}
			}
		}

		// Pass through the human-readable output.
		// In verbose mode, write immediately. In non-verbose mode,
		// buffer per-test output and only flush it on failure.
		if cfg.output != nil && ev.Output != "" && !passedThrough {
			if ev.Test != "" && !cfg.verbose {
				// Buffer test-specific output.
				key := ev.Package + "/" + ev.Test
				if ts, ok := spans[key]; ok {
					ts.bufferedOut.WriteString(ev.Output)
				}
			} else if cfg.verbose || strings.TrimSpace(ev.Output) != "PASS" {
				_, _ = io.WriteString(cfg.output, ev.Output)
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
				ps := &testSpan{
					span:      pkgSpan,
					spanStart: ev.Time,
					ctx:       pkgCtx,
				}
				if cfg.loggerProvider != nil {
					// Output that belongs to no test, such as a timeout
					// dump, is logged on the package span.
					pkgCtx = dagotel.WithLoggerProvider(pkgCtx, cfg.loggerProvider)
					ps.ctx = pkgCtx
					streams := dagotel.SpanStdio(pkgCtx, instrumentationLibrary)
					ps.streams = &streams
				}
				pkgSpans[ev.Package] = ps
			case "pass":
				if ps, ok := pkgSpans[ev.Package]; ok {
					if pkgRuns[ev.Package] == 0 {
						ps.span.SetStatus(codes.Ok, "no tests to run")
						ps.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
					} else {
						ps.span.SetStatus(codes.Ok, "")
						ps.span.SetAttributes(semconv.TestCaseResultStatusPass)
					}
					endPackageSpan(ps, ev)
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
				delete(pkgRuns, ev.Package)
				delete(dumps, ev.Package)
			case "fail":
				if d := dumps[ev.Package]; d != nil && cfg.output != nil && !cfg.verbose {
					d.flush(cfg.output)
				}
				finishUnfinishedTests(&cfg, spans, ev)
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Error, "package had failures")
					ps.span.SetAttributes(semconv.TestSuiteRunStatusFailure)
					endPackageSpan(ps, ev)
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
				delete(pkgRuns, ev.Package)
				delete(dumps, ev.Package)
			case "skip":
				if ps, ok := pkgSpans[ev.Package]; ok {
					ps.span.SetStatus(codes.Ok, "skipped")
					ps.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
					endPackageSpan(ps, ev)
					delete(pkgSpans, ev.Package)
				}
				delete(activeTests, ev.Package)
				delete(pkgRuns, ev.Package)
				delete(dumps, ev.Package)
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
				span:        span,
				spanStart:   ev.Time,
				ctx:         spanCtx,
				parentCtx:   parentCtx,
				packageName: ev.Package,
				testName:    ev.Test,
				spanName:    spanName,
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
			pkgRuns[ev.Package]++

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

					// A paused parallel test has been scheduled; count time from now
					// until test completion, linking to the test span so it adds to its
					// duration.
					_, span := tracer.Start(ts.ctx, "continue",
						trace.WithTimestamp(ev.Time),
						dagotel.Passthrough(),
						trace.WithLinks(trace.Link{SpanContext: linkSC}),
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
					_, _ = io.WriteString(ts.streams.Stdout, ev.Output)
				}
			}

		case "pass":
			if active := activeTests[ev.Package]; active != nil {
				delete(active, key)
			}
			if ts, ok := spans[key]; ok {
				if ts.streams != nil {
					_ = ts.streams.Close()
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
					_ = ts.streams.Close()
				}
				// Non-verbose: flush buffered output for failing tests.
				if cfg.output != nil && !cfg.verbose && ts.bufferedOut.Len() > 0 {
					_, _ = io.WriteString(cfg.output, ts.bufferedOut.String())
				}
				ts.span.SetStatus(codes.Error, "test failed")
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
					_ = ts.streams.Close()
				}
				// Non-verbose: discard buffered output for skipped tests.
				ts.span.SetStatus(codes.Ok, "test skipped")
				ts.span.SetAttributes(semconv.TestSuiteRunStatusSkipped)
				ts.span.End(trace.WithTimestamp(spanEndTime(ts, ev)))
				delete(spans, key)
			}
		}
	}

	// The stream ended without a package result, as when go test is killed:
	// copy what remains of a dump and close the package log streams.
	for pkg, d := range dumps {
		if d.active && cfg.output != nil && !cfg.verbose {
			d.flush(cfg.output)
		}
		delete(dumps, pkg)
	}
	for _, ps := range pkgSpans {
		if ps.streams != nil {
			_ = ps.streams.Close()
		}
	}

	return scanner.Err()
}
