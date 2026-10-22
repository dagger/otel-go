package main

import (
	"bufio"
	"bytes"
	"context"
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
func buildPrebuilt(t *testing.T, flags ...string) string {
	t.Helper()
	return buildTestBinary(t, prebuiltDir, ".", flags...)
}

// buildTestBinary compiles pkg's test binary with go test -c, in dir.
func buildTestBinary(t *testing.T, dir, pkg string, flags ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pkg.test")
	args := append([]string{"test", "-c", "-o", bin}, flags...)
	build := exec.Command("go", append(args, pkg)...)
	build.Dir = dir
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
		// go test -c flags the binary needs, if not the default build
		build []string
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
		{
			name:    "cover",
			build:   []string{"-cover"},
			goArgs:  []string{"-cover", "-run=^TestPass$", "."},
			binArgs: []string{"-test.run=^TestPass$"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := bin
			if tc.build != nil {
				bin = buildPrebuilt(t, tc.build...)
			}
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
	dir := t.TempDir()
	bin := filepath.Join(dir, "pkg.test")
	require.NoError(t, os.WriteFile(bin, []byte("binary"), 0o755))
	goFile := filepath.Join(dir, "x_test.go")
	require.NoError(t, os.WriteFile(goFile, []byte("package x"), 0o644))
	// A file named like a package in the working directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fmt"), []byte("not a binary"), 0o755))
	t.Chdir(dir)

	for _, tc := range []struct {
		name string
		args []string
		want *prebuilt
	}{
		{name: "binary", args: []string{bin, "-test.v", "-x"}, want: &prebuilt{binary: bin, args: []string{"-test.v", "-x"}}},
		{name: "json", args: []string{"-json", bin}, want: &prebuilt{binary: bin, args: []string{}, json: true}},
		{name: "relative", args: []string{"./pkg.test", "-test.v"}, want: &prebuilt{binary: "./pkg.test", args: []string{"-test.v"}}},
		{name: "package", args: []string{"-p", "example.com/x", "-json", bin}, want: &prebuilt{binary: bin, args: []string{}, json: true, pkg: "example.com/x"}},
		{name: "package equals", args: []string{"-p=example.com/x", bin}, want: &prebuilt{binary: bin, args: []string{}, pkg: "example.com/x"}},
		{name: "import path", args: []string{"fmt"}},
		{name: "go test -p", args: []string{"-p", "4", "./..."}},
		{name: "pattern", args: []string{"./..."}},
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
	for _, tc := range []struct {
		name  string
		args  []string
		value string
		ok    bool
	}{
		{name: "last wins", args: []string{"-test.timeout=1m", "-test.run", "X", "--test.timeout", "2m"}, value: "2m", ok: true},
		{name: "unset", args: []string{"-test.run=X"}},
		{name: "empty", args: []string{"-test.timeout="}, value: "", ok: true},
		{name: "after --", args: []string{"--", "-test.timeout=3m"}},
		{name: "after a positional argument", args: []string{"-test.v", "arg", "-test.timeout=3m"}},
		{name: "after a custom flag's value", args: []string{"-greeting", "hello", "-test.timeout=3m"}, value: "3m", ok: true},
		{name: "after a custom flag", args: []string{"-update=true", "-test.timeout=3m"}, value: "3m", ok: true},
		{name: "missing value", args: []string{"-test.timeout"}},
		{name: "a test flag's value looks like a flag", args: []string{"-test.run", "--", "-test.timeout=3m"}, value: "3m", ok: true},
		{name: "a test flag's value is the flag", args: []string{"-test.run", "-test.timeout=3m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, ok := lookupTestFlag(tc.args, "timeout")
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.value, value)
		})
	}
}

// TestPrebuiltAmbiguousTimeout runs a binary whose custom flag's value looks
// like -test.timeout: otelgotest leaves it to the binary, which keeps go
// test's default timeout.
func TestPrebuiltAmbiguousTimeout(t *testing.T) {
	bin := buildPrebuilt(t)
	out, code := runOtelgotest(t, hermeticEnv(), bin, "-test.run=^Test(Pass|Timeout)$", "-greeting", "-test.timeout=bogus")
	assert.Equal(t, 0, code, out)
	assert.Regexp(t, `(?m)^ok  \t`+regexp.QuoteMeta(prebuiltPkg)+`\t`, out)
	assert.NotContains(t, out, "no timeout")
}

// TestPrebuiltFuzzAfterFlagValue fuzzes with -test.fuzz after a test flag
// whose value is "--", which the binary parses as a value.
func TestPrebuiltFuzzAfterFlagValue(t *testing.T) {
	bin := buildPrebuilt(t)
	out, code := runOtelgotest(t, hermeticEnv("GOCACHE="+t.TempDir()),
		"-json", bin, "-test.skip", "--", "-test.run=^$", "-test.fuzz=^FuzzEcho$", "-test.fuzztime=5x")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "fuzz: elapsed")
}

// TestPrebuiltCoverDir runs a covered binary with the coverage directory go
// test gives it, also as GOCOVERDIR for the covered programs tests run.
func TestPrebuiltCoverDir(t *testing.T) {
	bin := buildPrebuilt(t, "-cover")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "default"},
		{name: "explicit", args: []string{"-test.gocoverdir=" + t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{bin, "-test.run=^TestCoverDir$", "-test.v"}, tc.args...)
			out, code := runOtelgotest(t, hermeticEnv("GOCOVERDIR="), args...)
			assert.Equal(t, 0, code, out)
			assert.Contains(t, out, "GOCOVERDIR is set")
		})
	}
}

// TestPrebuiltCoverDirFails stops before running a covered binary when the
// coverage directory cannot be created, as go test does.
func TestPrebuiltCoverDirFails(t *testing.T) {
	bin := buildPrebuilt(t, "-cover")
	out, code := runOtelgotest(t, hermeticEnv("TMPDIR="+filepath.Join(t.TempDir(), "missing")), bin, "-test.run=^TestCoverDir$", "-test.v")
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "failed to create temporary dir")
	assert.NotContains(t, out, "=== RUN")
}

// TestPrebuiltFuzz fuzzes with a prebuilt binary, which needs the fuzz cache
// directory go test passes.
func TestPrebuiltFuzz(t *testing.T) {
	bin := buildPrebuilt(t)
	out, code := runOtelgotest(t, hermeticEnv("GOCACHE="+t.TempDir()),
		"-json", bin, "-test.run=^$", "-test.fuzz=^FuzzEcho$", "-test.fuzztime=5x")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "fuzz: elapsed")
	assert.NotContains(t, out, "fuzzcachedir")
}

// TestPrebuiltStdoutFails checks a failing stdout does not hang otelgotest.
func TestPrebuiltStdoutFails(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	defer full.Close()
	bin := buildPrebuilt(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, otelgotestBin, "-json", bin, "-test.v", "-test.run=^Test(Pass|Sub)$")
	cmd.Dir = prebuiltDir
	cmd.Env = hermeticEnv()
	cmd.Stdout = full
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	require.NoError(t, ctx.Err(), "otelgotest hung: %s", stderr.String())
	assert.NoError(t, err, stderr.String())
}

// TestPrebuiltAdoptsSpans runs a test binary using oteltestctx, which asks
// otelgotest over OTEL_TEST_SOCKET for its test's span: spans the test
// starts are children of otelgotest's span for it.
func TestPrebuiltAdoptsSpans(t *testing.T) {
	bin := buildTestBinary(t, ".", "./testdata/adopt")
	otlp := startOTLPReceiver(t)
	cmd := exec.Command(otelgotestBin, bin, "-test.run=^TestAdopt$")
	cmd.Dir = "testdata/adopt"
	cmd.Env = hermeticEnv(
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT="+otlp.url,
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)

	spans := otlp.ended(t)
	inner, ok := spans["inner"]
	require.True(t, ok, "no span for the subtest: %v", spans)
	call, ok := spans["downstream-call"]
	require.True(t, ok, "no span from inside the test: %v", spans)
	assert.Equal(t, inner.GetTraceId(), call.GetTraceId())
	assert.Equal(t, inner.GetSpanId(), call.GetParentSpanId())
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
