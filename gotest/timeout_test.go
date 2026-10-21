package gotest_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dagger/otel-go/gotest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const timeoutPkg = "example.com/pkg"

// timeoutEvents reproduces how go test -json reports a -timeout panic: the
// panic, its running-tests list and the goroutine dump are attributed to the
// test that reported last (TestPassed, whose pass is held back until the
// final FAIL line), and the still-running TestHung and never-continued
// TestPaused get no result of their own before the package fails.
func timeoutEvents(dump ...string) []gotest.TestEvent {
	now := time.Now()
	ev := func(action, test, output string) gotest.TestEvent {
		return gotest.TestEvent{Time: now, Action: action, Package: timeoutPkg, Test: test, Output: output}
	}
	events := []gotest.TestEvent{
		ev("start", "", ""),
		ev("run", "TestHung", ""),
		ev("output", "TestHung", "=== RUN   TestHung\n"),
		ev("output", "TestHung", "hung_test.go:10: waiting for the CLI\n"),
		ev("run", "TestPaused", ""),
		ev("output", "TestPaused", "paused output\n"),
		ev("pause", "TestPaused", ""),
		ev("run", "TestPassed", ""),
		ev("output", "TestPassed", "passed test output\n"),
		ev("output", "TestPassed", "--- PASS: TestPassed (0.01s)\n"),
	}
	if len(dump) == 0 {
		dump = []string{
			"panic: test timed out after 20m0s\n",
			"\trunning tests:\n",
			"\t\tTestHung (18m41s)\n",
			"goroutine 1 [running]:\n",
			"testing.(*M).startAlarm.func1()\n",
			"goroutine 7 [chan receive, 18 minutes]:\n",
			"example.com/pkg.TestHung()\n",
		}
	}
	for _, out := range dump {
		events = append(events, ev("output", "TestPassed", out))
	}
	return append(events,
		ev("pass", "TestPassed", ""),
		ev("output", "", "FAIL\n"),
		ev("output", "", "FAIL\texample.com/pkg\t1200.462s\n"),
		ev("fail", "", ""),
	)
}

func runWithOutput(t *testing.T, events []gotest.TestEvent, opts ...gotest.Option) ([]sdktrace.ReadOnlySpan, string, *recordingLogExporter) {
	t.Helper()
	logExporter := &recordingLogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))
	t.Cleanup(func() {
		require.NoError(t, lp.Shutdown(context.Background()))
	})
	spans, out := runWithoutLogs(t, events, append(opts, gotest.WithLoggerProvider(lp))...)
	require.NoError(t, lp.ForceFlush(context.Background()))
	return spans, out, logExporter
}

func runWithoutLogs(t *testing.T, events []gotest.TestEvent, opts ...gotest.Option) ([]sdktrace.ReadOnlySpan, string) {
	t.Helper()
	var out bytes.Buffer
	spans := runEvents(t, events, append(opts, gotest.WithOutput(&out))...)
	return spans, out.String()
}

func testEvent(pkg string) func(action, test, output string) gotest.TestEvent {
	now := time.Now()
	return func(action, test, output string) gotest.TestEvent {
		return gotest.TestEvent{Time: now, Action: action, Package: pkg, Test: test, Output: output}
	}
}

// largeDump is a timeout panic whose goroutine dump is over twice
// timeoutStacksLimit.
func largeDump(t *testing.T) []string {
	t.Helper()
	dump := []string{"panic: test timed out after 20m0s\n", "\trunning tests:\n", "\t\tTestHung (18m41s)\n"}
	full := 0
	for i := range 2000 {
		line := fmt.Sprintf("goroutine %d [select]:\nexample.com/pkg.worker(%d)\n\t/src/worker.go:42 +0x1f\n", i, i)
		full += len(line)
		dump = append(dump, line)
	}
	require.Greater(t, full, 128<<10)
	return dump
}

func logsOn(records []sdklog.Record, span sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, rec := range records {
		if rec.SpanID() == span.SpanContext().SpanID() {
			b.WriteString(rec.Body().AsString())
		}
	}
	return b.String()
}

func TestTimeoutPanicReachesOutput(t *testing.T) {
	t.Parallel()

	spans, out, logs := runWithOutput(t, timeoutEvents())

	assert.Equal(t, 1, strings.Count(out, "panic: test timed out after 20m0s\n"), out)
	assert.Contains(t, out, "\trunning tests:\n\t\tTestHung (18m41s)\n")
	assert.Contains(t, out, "goroutine 7 [chan receive, 18 minutes]:\nexample.com/pkg.TestHung()\n")
	assert.Contains(t, out,
		"--- UNFINISHED: TestHung (did not finish: package failed)\n=== RUN   TestHung\nhung_test.go:10: waiting for the CLI\n")
	assert.Contains(t, out, "--- UNFINISHED: TestPaused (did not finish: package failed)\npaused output\n")
	assert.Less(t, strings.Index(out, "TestHung (did not finish"), strings.Index(out, "TestPaused (did not finish"),
		"unfinished tests are reported in name order")
	assert.NotContains(t, out, "passed test output", "a passing test's output stays quiet")
	assert.NotContains(t, out, "truncated")

	pkg := findSpan(spans, timeoutPkg)
	require.NotNil(t, pkg)
	assert.Equal(t, codes.Error, pkg.Status().Code)
	assert.Contains(t, logsOn(logs.Records(), pkg), "panic: test timed out after 20m0s\n\trunning tests:\n")

	hung := findSpansByTestCaseName(spans, "TestHung")
	require.Len(t, hung, 1, "the running test's span is ended")
	assert.Equal(t, codes.Error, hung[0].Status().Code)
	assert.Equal(t, "did not finish: package failed", hung[0].Status().Description)

	// The paused test's span ended at its pause and is left as it was; the
	// recorder cannot count End calls (End is idempotent), only spans.
	paused := findSpansByTestCaseName(spans, "TestPaused")
	require.Len(t, paused, 1)
	assert.NotEqual(t, codes.Error, paused[0].Status().Code)

	passed := findSpansByTestCaseName(spans, "TestPassed")
	require.Len(t, passed, 1)
	assert.Equal(t, codes.Ok, passed[0].Status().Code)
	assert.NotContains(t, logsOn(logs.Records(), passed[0]), "test timed out",
		"the dump is not logged on the test it was attributed to")
}

func TestTimeoutPanicVerboseIsNotRepeated(t *testing.T) {
	t.Parallel()

	_, out, _ := runWithOutput(t, timeoutEvents(), gotest.WithVerbose(true))

	assert.Equal(t, 1, strings.Count(out, "panic: test timed out after 20m0s\n"), out)
	assert.Equal(t, 1, strings.Count(out, "goroutine 7 [chan receive, 18 minutes]:\n"), out)
	assert.Equal(t, 1, strings.Count(out, "hung_test.go:10: waiting for the CLI\n"), out)
	assert.NotContains(t, out, "--- UNFINISHED", "verbose output was already printed")
}

func TestTimeoutPanicSplitAcrossEvents(t *testing.T) {
	t.Parallel()

	spans, out, logs := runWithOutput(t, timeoutEvents(
		"panic: test ti",
		"med out after 1s\n\trunning tests:\n\t\tTestHung (1s)\n",
		"goroutine 1 [running]:\n",
	))

	assert.Equal(t, 1, strings.Count(out, "panic: test timed out after 1s\n"), out)
	assert.Contains(t, out, "\trunning tests:\n\t\tTestHung (1s)\n")
	assert.Contains(t, out, "goroutine 1 [running]:\n")

	pkg := findSpan(spans, timeoutPkg)
	require.NotNil(t, pkg)
	assert.True(t, strings.HasPrefix(logsOn(logs.Records(), pkg), "panic: test timed out after 1s\n\trunning tests:\n"),
		"the package log holds the whole signature line: %q", logsOn(logs.Records(), pkg))
}

// A split signature line whose first fragment was attributed to a test that
// never finishes is printed once, in the dump, and not again with that
// test's buffered output.
func TestTimeoutPanicSplitFromUnfinishedTest(t *testing.T) {
	t.Parallel()

	ev := testEvent(timeoutPkg)
	_, out, _ := runWithOutput(t, []gotest.TestEvent{
		ev("start", "", ""),
		ev("run", "TestHung", ""),
		ev("output", "TestHung", "hung output\n"),
		ev("output", "TestHung", "panic: test timed o"),
		ev("output", "TestHung", "ut after 1s\n\trunning tests:\n\t\tTestHung (1s)\ngoroutine 1 [running]:\n"),
		ev("output", "", "FAIL\n"),
		ev("fail", "", ""),
	})

	assert.Equal(t, 1, strings.Count(out, "panic: test timed o"), out)
	assert.Contains(t, out, "panic: test timed out after 1s\n")
	assert.Contains(t, out, "--- UNFINISHED: TestHung (did not finish: package failed)\nhung output\n")
}

func TestTimeoutPanicDumpIsBounded(t *testing.T) {
	t.Parallel()

	spans, out, logs := runWithOutput(t, timeoutEvents(largeDump(t)...))

	assert.Contains(t, out, "panic: test timed out after 20m0s\n\trunning tests:\n\t\tTestHung (18m41s)\n")
	assert.Contains(t, out, "goroutine dump truncated after ")
	assert.Contains(t, out, "full dump in the package span logs")
	assert.Less(t, len(out), 72<<10, "the copied dump is capped")
	assert.Contains(t, out, "--- UNFINISHED: TestHung", "the unfinished test is still reported after truncation")

	pkg := findSpan(spans, timeoutPkg)
	require.NotNil(t, pkg)
	assert.Contains(t, logsOn(logs.Records(), pkg), "goroutine 1999 [select]:\n", "the span logs keep the whole dump")
}

// Without a logger provider nothing retains the dump, so it is copied whole
// rather than truncated with a pointer to logs that do not exist.
func TestTimeoutPanicDumpWithoutLogsIsNotTruncated(t *testing.T) {
	t.Parallel()

	_, out := runWithoutLogs(t, timeoutEvents(largeDump(t)...))

	assert.NotContains(t, out, "truncated")
	assert.Contains(t, out, "goroutine 1999 [select]:\n")
	assert.Contains(t, out, "--- UNFINISHED: TestHung")
}

// Timeout output attributed to the package itself (an empty Test) is routed
// and capped the same way, and the package's result lines stay visible. This
// covers the routing for package-tagged output; it does not reproduce a hung
// TestMain, which the in-binary alarm does not cover.
func TestTimeoutPanicAttributedToPackageIsBounded(t *testing.T) {
	t.Parallel()

	ev := testEvent(timeoutPkg)
	events := []gotest.TestEvent{ev("start", "", "")}
	for _, out := range largeDump(t) {
		events = append(events, ev("output", "", out))
	}
	events = append(events,
		ev("output", "", "FAIL\texample.com/pkg\t1200.462s\n"),
		ev("fail", "", ""),
	)
	spans, out, logs := runWithOutput(t, events)

	assert.Contains(t, out, "panic: test timed out after 20m0s\n\trunning tests:\n\t\tTestHung (18m41s)\n")
	assert.Contains(t, out, "full dump in the package span logs")
	assert.Less(t, len(out), 72<<10, "the copied dump is capped")
	assert.True(t, strings.HasSuffix(out, "FAIL\texample.com/pkg\t1200.462s\n"), "the package result stays visible")

	pkg := findSpan(spans, timeoutPkg)
	require.NotNil(t, pkg)
	assert.Contains(t, logsOn(logs.Records(), pkg), "goroutine 1999 [select]:\n")
}

// A package whose name is a prefix of another's times out while the other
// is still running: only the failing package's tests are finished, and the
// dump stays with it.
func TestTimeoutPanicInterleavedPackages(t *testing.T) {
	t.Parallel()

	p, other := testEvent("p"), testEvent("p/other")
	spans, out, logs := runWithOutput(t, []gotest.TestEvent{
		p("start", "", ""),
		other("start", "", ""),
		p("run", "TestHung", ""),
		other("run", "TestOther", ""),
		p("output", "TestHung", "hung output\n"),
		other("output", "TestOther", "other output\n"),
		p("output", "TestHung", "panic: test timed out after 1s\n\trunning tests:\n\t\tTestHung (1s)\n"),
		other("output", "TestOther", "more other output\n"),
		p("output", "TestHung", "goroutine 1 [running]:\n"),
		p("output", "", "FAIL\tp\t1.0s\n"),
		p("fail", "", ""),
		other("pass", "TestOther", ""),
		other("output", "", "ok  \tp/other\t1.1s\n"),
		other("pass", "", ""),
	})

	assert.Equal(t, 1, strings.Count(out, "panic: test timed out after 1s\n"), out)
	assert.Contains(t, out, "--- UNFINISHED: TestHung (did not finish: package failed)\nhung output\n")
	assert.NotContains(t, out, "TestOther")
	assert.NotContains(t, out, "other output")
	assert.Contains(t, out, "ok  \tp/other\t1.1s\n")

	otherTest := findSpanByTestCaseName(spans, "TestOther")
	require.NotNil(t, otherTest)
	assert.Equal(t, codes.Ok, otherTest.Status().Code)
	otherPkg := findSpan(spans, "p/other")
	require.NotNil(t, otherPkg)
	assert.Equal(t, codes.Ok, otherPkg.Status().Code)
	assert.NotContains(t, logsOn(logs.Records(), otherPkg), "test timed out")
	pPkg := findSpan(spans, "p")
	require.NotNil(t, pPkg)
	assert.Contains(t, logsOn(logs.Records(), pPkg), "panic: test timed out after 1s\n")
}

// A parallel subtest that continued after its pause, and its running parent,
// are both finished: the subtest's continuation span and the parent's span
// end in error, and their output is reported parent first.
func TestTimeoutFinishesContinuedSubtestAndParent(t *testing.T) {
	t.Parallel()

	ev := testEvent(timeoutPkg)
	spans, out, _ := runWithOutput(t, []gotest.TestEvent{
		ev("start", "", ""),
		ev("run", "TestParent", ""),
		ev("output", "TestParent", "parent output\n"),
		ev("run", "TestParent/sub", ""),
		ev("pause", "TestParent/sub", ""),
		ev("cont", "TestParent/sub", ""),
		ev("output", "TestParent/sub", "sub output\n"),
		ev("output", "TestParent/sub", "panic: test timed out after 1s\n\trunning tests:\n\t\tTestParent/sub (1s)\n"),
		ev("output", "TestParent/sub", "goroutine 1 [running]:\n"),
		ev("output", "", "FAIL\n"),
		ev("fail", "", ""),
	})

	parentAt := strings.Index(out, "--- UNFINISHED: TestParent (did not finish: package failed)\nparent output\n")
	subAt := strings.Index(out, "--- UNFINISHED: TestParent/sub (did not finish: package failed)\nsub output\n")
	require.GreaterOrEqual(t, parentAt, 0, out)
	require.GreaterOrEqual(t, subAt, 0, out)
	assert.Less(t, parentAt, subAt)

	parent := findSpanByTestCaseName(spans, "TestParent")
	require.NotNil(t, parent)
	assert.Equal(t, codes.Error, parent.Status().Code)
	assert.Equal(t, "did not finish: package failed", parent.Status().Description)

	sub := findSpansByTestCaseName(spans, "TestParent/sub")
	require.Len(t, sub, 1, "the paused setup span")
	cont := findContinuationSpanFor(spans, sub[0])
	require.NotNil(t, cont, "the continuation span is ended")
	assert.Equal(t, codes.Error, cont.Status().Code)
	assert.Equal(t, "did not finish: package failed", cont.Status().Description)
}

func TestPassingTestPanicTextIsNotPromoted(t *testing.T) {
	t.Parallel()

	now := time.Now()
	ev := func(action, test, output string) gotest.TestEvent {
		return gotest.TestEvent{Time: now, Action: action, Package: timeoutPkg, Test: test, Output: output}
	}
	spans, out, logs := runWithOutput(t, []gotest.TestEvent{
		ev("start", "", ""),
		ev("run", "TestRecovers", ""),
		ev("output", "TestRecovers", "panic: something went wrong [recovered]\n"),
		ev("output", "TestRecovers", "goroutine 9 [running]:\n"),
		ev("pass", "TestRecovers", ""),
		ev("output", "", "PASS\n"),
		ev("pass", "", ""),
	})

	assert.NotContains(t, out, "something went wrong")
	assert.NotContains(t, out, "goroutine 9")
	pkg := findSpan(spans, timeoutPkg)
	require.NotNil(t, pkg)
	assert.Equal(t, codes.Ok, pkg.Status().Code)
	assert.Empty(t, logsOn(logs.Records(), pkg), "ordinary panic text is not routed to the package")
}
