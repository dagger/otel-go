package gotest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestPackageFailurePreservesPendingOutput(t *testing.T) {
	events := []TestEvent{
		{Action: "start", Package: "p"},
		{Action: "start", Package: "p/other"},
		{Action: "run", Package: "p/other", Test: "TestOther"},
		{Action: "output", Package: "p/other", Test: "TestOther", Output: "other package output\n"},
		{Action: "run", Package: "p", Test: "TestPass"},
		{Action: "output", Package: "p", Test: "TestPass", Output: "passing output\n"},
		{Action: "pass", Package: "p", Test: "TestPass"},
		{Action: "run", Package: "p", Test: "TestSkip"},
		{Action: "output", Package: "p", Test: "TestSkip", Output: "skipped output\n"},
		{Action: "skip", Package: "p", Test: "TestSkip"},
		{Action: "run", Package: "p", Test: "TestFailed"},
		{Action: "output", Package: "p", Test: "TestFailed", Output: "ordinary failure\n"},
		{Action: "fail", Package: "p", Test: "TestFailed"},
		{Action: "run", Package: "p", Test: "TestPaused"},
		{Action: "output", Package: "p", Test: "TestPaused", Output: "paused output\n"},
		{Action: "pause", Package: "p", Test: "TestPaused"},
		{Action: "run", Package: "p", Test: "TestTimeout"},
		{Action: "output", Package: "p", Test: "TestTimeout", Output: "panic: test timed out after 1s\ngoroutine 1 [chan receive]:\n"},
		{Action: "output", Package: "p", Output: "FAIL p\n"},
		{Action: "fail", Package: "p"},
		{Action: "pass", Package: "p/other", Test: "TestOther"},
		{Action: "pass", Package: "p/other"},
	}
	for _, verbose := range []bool{false, true} {
		name := "quiet"
		if verbose {
			name = "verbose"
		}
		t.Run(name, func(t *testing.T) {
			var input, output bytes.Buffer
			enc := json.NewEncoder(&input)
			for _, ev := range events {
				if err := enc.Encode(ev); err != nil {
					t.Fatal(err)
				}
			}
			if err := Run(t.Context(), &input, noop.NewTracerProvider(), WithOutput(&output), WithVerbose(verbose)); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"ordinary failure\n", "paused output\n", "panic: test timed out after 1s\n", "goroutine 1 [chan receive]:\n"} {
				if n := strings.Count(output.String(), want); n != 1 {
					t.Fatalf("count(%q)=%d, want 1; output:\n%s", want, n, &output)
				}
			}
			if !verbose {
				for _, excluded := range []string{"other package output", "passing output", "skipped output"} {
					if strings.Contains(output.String(), excluded) {
						t.Fatalf("unexpected %q in %s", excluded, &output)
					}
				}
			}
		})
	}
}
