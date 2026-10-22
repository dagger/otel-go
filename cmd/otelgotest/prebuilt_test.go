package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The package otelgotest runs in these tests, as a package and prebuilt.
const (
	prebuiltDir = "testdata/prebuilt"
	prebuiltPkg = "example.com/prebuilt"
)

// otelgotestBin is this command, built once by TestMain.
var otelgotestBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otelgotest-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	otelgotestBin = filepath.Join(dir, "otelgotest")
	build := exec.Command("go", "build", "-o", otelgotestBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build otelgotest:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// buildPrebuilt compiles the testdata package's test binary.
func buildPrebuilt(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "prebuilt.test")
	build := exec.Command("go", "test", "-c", "-o", bin, ".")
	build.Dir = prebuiltDir
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go test -c: %s", out)
	return bin
}

// hermeticEnv is the environment without any telemetry configuration
// inherited from whatever runs these tests, plus env.
func hermeticEnv(env ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "OTEL_") || strings.HasPrefix(kv, "TRACEPARENT=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, env...)
}

// runOtelgotest runs otelgotest in the testdata package's directory and
// returns its output and exit status.
func runOtelgotest(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(otelgotestBin, args...)
	cmd.Dir = prebuiltDir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode()
	}
	require.NoError(t, err)
	return out.String(), 0
}

// otlpReceiver collects the spans otelgotest exports over OTLP/HTTP.
type otlpReceiver struct {
	url   string
	mu    sync.Mutex
	spans map[string]*tracepb.Span // by span ID; live updates replace
}

func startOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{spans: map[string]*tracepb.Span{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var export coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &export); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		for _, rs := range export.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					r.spans[hex.EncodeToString(span.GetSpanId())] = span
				}
			}
		}
		r.mu.Unlock()
		resp, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL + "/v1/traces"
	return r
}

// ended returns the ended spans by name; names must be unique.
func (r *otlpReceiver) ended(t *testing.T) map[string]*tracepb.Span {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	byName := map[string]*tracepb.Span{}
	for _, span := range r.spans {
		if span.GetEndTimeUnixNano() == 0 {
			continue
		}
		require.NotContains(t, byName, span.GetName(), "duplicate span name")
		byName[span.GetName()] = span
	}
	return byName
}

// TestPrebuiltBinary runs a test binary built by go test -c, and checks it
// reports per-test spans, output and exit status as go test would.
func TestPrebuiltBinary(t *testing.T) {
	bin := buildPrebuilt(t)
	otlp := startOTLPReceiver(t)

	out, code := runOtelgotest(t,
		hermeticEnv(
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+otlp.url,
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
		),
		bin, "-test.run=^Test(Pass|Fail|Skip|Sub|Workdir|Flag)$", "-greeting=hello",
	)
	t.Logf("output:\n%s", out)

	assert.Equal(t, 1, code, "a failing test fails the run")
	assert.Contains(t, out, "--- FAIL: TestFail")
	assert.Contains(t, out, "expected failure")
	assert.NotContains(t, out, "passing output", "without -test.v, passing tests' output is hidden")
	assert.Regexp(t, `(?m)^FAIL\t`+regexp.QuoteMeta(prebuiltPkg)+`\t\d+\.\d+s$`, out)

	spans := otlp.ended(t)
	statuses := map[string]tracepb.Status_StatusCode{}
	for name, span := range spans {
		statuses[name] = span.GetStatus().GetCode()
	}
	assert.Equal(t, map[string]tracepb.Status_StatusCode{
		prebuiltPkg:   tracepb.Status_STATUS_CODE_ERROR,
		"TestPass":    tracepb.Status_STATUS_CODE_OK,
		"TestFail":    tracepb.Status_STATUS_CODE_ERROR,
		"TestSkip":    tracepb.Status_STATUS_CODE_OK,
		"TestSub":     tracepb.Status_STATUS_CODE_OK,
		"child":       tracepb.Status_STATUS_CODE_OK,
		"TestWorkdir": tracepb.Status_STATUS_CODE_OK,
		"TestFlag":    tracepb.Status_STATUS_CODE_OK,
	}, statuses)
	if pkg, ok := spans[prebuiltPkg]; ok {
		for _, name := range []string{"TestPass", "TestFail", "TestSkip", "TestSub", "TestWorkdir", "TestFlag"} {
			if span, ok := spans[name]; ok {
				assert.Equal(t, pkg.GetSpanId(), span.GetParentSpanId(), "%s is a child of the package span", name)
			}
		}
	}
	if sub, ok := spans["TestSub"]; ok {
		if child, ok := spans["child"]; ok {
			assert.Equal(t, sub.GetSpanId(), child.GetParentSpanId(), "a subtest is a child of its test")
		}
	}
}

// TestPrebuiltMatchesGoTest checks a prebuilt binary's go test -json events
// match go test's own for the same tests, apart from times.
func TestPrebuiltMatchesGoTest(t *testing.T) {
	bin := buildPrebuilt(t)
	for _, tc := range []struct {
		name string
		// go test flags, and the test binary flags go test passes for them
		goArgs, binArgs []string
	}{
		{
			name:    "pass fail skip",
			goArgs:  []string{"-run=^Test(Pass|Fail|Skip|Sub|Workdir|Flag)$", ".", "-greeting=hello"},
			binArgs: []string{"-test.run=^Test(Pass|Fail|Skip|Sub|Workdir|Flag)$", "-greeting=hello"},
		},
		{
			name:    "verbose",
			goArgs:  []string{"-v", "-run=^Test(Pass|Fail|Skip|Sub)$", "."},
			binArgs: []string{"-test.v=true", "-test.run=^Test(Pass|Fail|Skip|Sub)$"},
		},
		{
			name:    "pass",
			goArgs:  []string{"-run=^TestPass$", "-timeout=30s", "."},
			binArgs: []string{"-test.run=^TestPass$", "-test.timeout=30s"},
		},
		{
			name:    "no tests to run",
			goArgs:  []string{"-run=^TestNothing$", "."},
			binArgs: []string{"-test.run=^TestNothing$"},
		},
		{
			name:    "exit",
			goArgs:  []string{"-run=^TestExit$", ".", "-exit=3"},
			binArgs: []string{"-test.run=^TestExit$", "-exit=3"},
		},
		{
			name:    "bad flag",
			goArgs:  []string{"-run=^TestPass$", ".", "-nosuchflag"},
			binArgs: []string{"-test.run=^TestPass$", "-nosuchflag"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Without a telemetry endpoint, go test -json's events are go
			// test's own: otelgotest runs go test directly.
			goOut, goCode := runOtelgotest(t, hermeticEnv(), append([]string{"-json", "-count=1"}, tc.goArgs...)...)
			binOut, binCode := runOtelgotest(t, hermeticEnv(), append([]string{"-json", bin, "-test.count=1"}, tc.binArgs...)...)
			assert.Equal(t, goCode, binCode, "exit status")
			assert.Equal(t, normalizeEvents(t, goOut), normalizeEvents(t, binOut))
		})
	}
}

// durations matches the times in go test output.
var durations = regexp.MustCompile(`\d+\.\d+s`)

// usage matches the test binary's path in its usage message, which go test
// builds in a temporary directory.
var usage = regexp.MustCompile(`^Usage of .*:\n$`)

// normalizeEvents parses a go test -json stream, without times.
func normalizeEvents(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		var ev map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &ev), "event %q", scanner.Text())
		delete(ev, "Time")
		delete(ev, "Elapsed")
		if out, ok := ev["Output"].(string); ok {
			out = durations.ReplaceAllString(out, "N.NNs")
			ev["Output"] = usage.ReplaceAllString(out, "Usage of BINARY:\n")
		}
		events = append(events, ev)
	}
	require.NoError(t, scanner.Err())
	return events
}

func TestParsePrebuilt(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "pkg.test")
	require.NoError(t, os.WriteFile(bin, []byte("binary"), 0o755))
	goFile := filepath.Join(t.TempDir(), "x_test.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package x"), 0o644))

	for _, tc := range []struct {
		name string
		args []string
		want *prebuilt
	}{
		{name: "binary", args: []string{bin, "-test.v", "-x"}, want: &prebuilt{binary: bin, args: []string{"-test.v", "-x"}}},
		{name: "json", args: []string{"-json", bin}, want: &prebuilt{binary: bin, args: []string{}, json: true}},
		{name: "package", args: []string{"./..."}},
		{name: "flags first", args: []string{"-v", bin}},
		{name: "directory", args: []string{t.TempDir()}},
		{name: "go file", args: []string{goFile}},
		{name: "missing", args: []string{filepath.Join(t.TempDir(), "missing.test")}},
		{name: "none", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePrebuilt(tc.args)
			assert.Equal(t, tc.want != nil, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLookupTestFlag(t *testing.T) {
	args := []string{"-test.timeout=1m", "-test.run", "X", "--test.timeout", "2m", "-test.bench=", "--", "-test.timeout=3m"}
	v, ok := lookupTestFlag(args, "timeout")
	assert.True(t, ok)
	assert.Equal(t, "2m", v)
	v, ok = lookupTestFlag(args, "bench")
	assert.True(t, ok)
	assert.Empty(t, v)
	_, ok = lookupTestFlag(args, "fuzz")
	assert.False(t, ok)
}

// killTimeouts matches go test's backstop (cmd/go/internal/test).
func TestKillTimeouts(t *testing.T) {
	for _, tc := range []struct {
		timeout     time.Duration
		benchOrFuzz bool
		kill, wait  time.Duration
	}{
		{timeout: 20 * time.Minute, kill: 22 * time.Minute, wait: 2 * time.Minute},
		{timeout: 10 * time.Minute, kill: 11 * time.Minute, wait: time.Minute},
		{timeout: 30 * time.Second, kill: 90 * time.Second, wait: 5 * time.Second},
		{timeout: 0},
		{timeout: time.Minute, benchOrFuzz: true},
	} {
		kill, wait := killTimeouts(tc.timeout, tc.benchOrFuzz)
		assert.Equal(t, tc.kill, kill, "kill after %v", tc.timeout)
		assert.Equal(t, tc.wait, wait, "wait delay after %v", tc.timeout)
	}
}
