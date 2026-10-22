package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dagger/otel-go/gotest"
	"github.com/dagger/otel-go/internal/test2json"
	"go.opentelemetry.io/otel/trace"
)

// A prebuilt test binary to run, as in
//
//	otelgotest [-json] [-p importpath] ./path/to/pkg.test [test binary flags]
//
// otelgotest runs the binary the way go test -json runs a test binary it
// has built, and reports the same events and output. The test binary flags
// are the binary's own (-test.run, -test.timeout, custom test flags, ...),
// not go test's. As with go test, run it from the package's directory.
type prebuilt struct {
	binary string
	args   []string
	// json writes the go test -json event stream to stdout instead of
	// human-readable output.
	json bool
	// pkg is the import path of the package the binary tests, if given;
	// by default it is read from the binary's build info.
	pkg string
}

// go test's default -timeout, which the test binary itself does not have.
const defaultTestTimeout = 10 * time.Minute

// parsePrebuilt reports whether args name a prebuilt test binary rather than
// go test arguments: an existing regular file, named by a path that starts
// with ./, ../ or /, and is not a .go file, optionally after -json and -p.
// go test reads such a path as a package directory, so no valid go test
// command line has that shape.
func parsePrebuilt(args []string) (*prebuilt, bool) {
	p := &prebuilt{}
	i := 0
flags:
	for ; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-json" || arg == "-json=true":
			p.json = true
		case arg == "-json=false":
			p.json = false
		case arg == "-p" && i+1 < len(args):
			i++
			p.pkg = args[i]
		case strings.HasPrefix(arg, "-p="):
			p.pkg = strings.TrimPrefix(arg, "-p=")
		default:
			break flags
		}
	}
	if i == len(args) {
		return nil, false
	}
	bin := args[i]
	if !isLocalPath(bin) || strings.HasSuffix(bin, ".go") {
		return nil, false
	}
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	p.binary = bin
	p.args = args[i+1:]
	return p, true
}

// isLocalPath reports whether arg is a file system path rather than an
// import path, as go does: one that starts with ./, ../ or /.
func isLocalPath(arg string) bool {
	if filepath.IsAbs(arg) {
		return true
	}
	for _, prefix := range []string{".", ".."} {
		if strings.HasPrefix(arg, prefix+"/") || strings.HasPrefix(arg, prefix+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// runPrebuilt runs a prebuilt test binary as go test -json runs a test
// binary (see runTestActor.Act in cmd/go/internal/test), converts its output
// with the same converter, and processes the events with gotest.Run. env is
// added to the binary's environment. It returns go test's exit status.
func runPrebuilt(ctx context.Context, p *prebuilt, tp trace.TracerProvider, opts []gotest.Option, env []string, stdout io.Writer) int {
	info, _ := buildinfo.ReadFile(p.binary)
	pkg := p.pkg
	if pkg == "" {
		pkg = testBinaryPackage(p.binary, info)
	}
	userArgs := p.args

	// go test passes -test.paniconexit0, -test.fuzzcachedir when fuzzing,
	// -test.gocoverdir when covering, then -test.v=test2json and its default
	// timeout, then the user's flags. Like go test -json -v, a user's
	// -test.v overrides -test.v=test2json; test2json copes.
	args := []string{"-test.paniconexit0"}
	fuzz, _ := lookupTestFlag(userArgs, "fuzz")
	if _, ok := lookupTestFlag(userArgs, "fuzzcachedir"); fuzz != "" && !ok {
		args = append(args, "-test.fuzzcachedir="+fuzzCacheDir(pkg))
	}
	if _, ok := lookupTestFlag(userArgs, "gocoverdir"); builtWithCover(info) && !ok {
		// As go test does, also set GOCOVERDIR, for covered binaries the
		// tests run.
		dir, err := os.MkdirTemp("", "otelgotest-gocoverdir")
		if err == nil {
			defer func() { _ = os.RemoveAll(dir) }()
			args = append(args, "-test.gocoverdir="+dir)
			env = append(env, "GOCOVERDIR="+dir)
		}
	}
	args = append(args, "-test.v=test2json")
	timeout := defaultTestTimeout
	if v, ok := lookupTestFlag(userArgs, "timeout"); !ok {
		args = append(args, fmt.Sprintf("-test.timeout=%v", timeout))
	} else if d, err := time.ParseDuration(v); err == nil {
		timeout = d
	} else {
		// Not a timeout after all, or one the binary will reject: keep go
		// test's default in place, which the binary's own flag overrides.
		args = append(args, fmt.Sprintf("-test.timeout=%v", timeout))
	}
	args = append(args, userArgs...)
	bench, _ := lookupTestFlag(userArgs, "bench")
	killTimeout, waitDelay := killTimeouts(timeout, bench != "" || fuzz != "")

	stream, events := io.Pipe()
	r := io.Reader(stream)
	if p.json {
		r = io.TeeReader(stream, stdout)
	} else {
		opts = append(opts, gotest.WithOutput(stdout), gotest.WithVerbose(hasVerboseFlag(userArgs)))
	}
	done := make(chan error, 1)
	go func() {
		err := gotest.Run(ctx, r, tp, opts...)
		// Keep the converter from blocking if Run stopped early, even when
		// what stopped it was writing to stdout.
		_, _ = io.Copy(io.Discard, stream)
		done <- err
	}()

	conv := test2json.NewConverter(events, pkg, test2json.Timestamp)
	out := newOutputTracker(conv)

	if killTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, killTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.Env = append(os.Environ(), env...)
	if wd, err := os.Getwd(); err == nil {
		cmd.Env = append(cmd.Env, "PWD="+wd)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	var cancelKilled, cancelSignaled bool
	cmd.Cancel = func() error {
		if signalTrace == nil {
			err := cmd.Process.Kill()
			if err == nil {
				cancelKilled = true
			}
			return err
		}
		// Send a quit signal in the hope that the program will print a
		// stack trace and exit.
		err := cmd.Process.Signal(signalTrace)
		if err == nil {
			cancelSignaled = true
		}
		return err
	}
	cmd.WaitDelay = waitDelay

	t0 := time.Now()
	err := cmd.Run()
	elapsed := fmt.Sprintf("%.3fs", time.Since(t0).Seconds())

	if err == nil {
		norun := ""
		if out.noTests {
			norun = " [no tests to run]"
		}
		coverage := ""
		if out.coverage != "" && builtWithCover(info) {
			coverage = "\tcoverage: " + out.coverage
		}
		if out.n > 0 && out.last != '\n' {
			_, _ = out.Write([]byte("\n"))
		}
		_, _ = fmt.Fprintf(out, "ok  \t%s\t%s%s%s\n", pkg, elapsed, coverage, norun)
	} else {
		n, last := out.n, out.last
		if cancelSignaled {
			_, _ = fmt.Fprintf(out, "*** Test killed with %v: ran too long (%v).\n", signalTrace, killTimeout)
		} else if cancelKilled {
			_, _ = fmt.Fprintf(out, "*** Test killed: ran too long (%v).\n", killTimeout)
		} else if errors.Is(err, exec.ErrWaitDelay) {
			_, _ = fmt.Fprintf(out, "*** Test I/O incomplete %v after exiting.\n", cmd.WaitDelay)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || !ee.Exited() || n == 0 {
			// If there was no test output, print the exit status so that
			// the reason for failure is clear.
			_, _ = fmt.Fprintf(out, "%s\n", err)
		} else if last != '\n' {
			_, _ = out.Write([]byte("\n"))
		}
		// test2json reports a pass unless FAIL starts a line, which a test
		// that panics or exits may not print, so go test prints it.
		_, _ = fmt.Fprintf(out, "\x16FAIL\t%s\t%s\n", pkg, elapsed)
	}
	conv.Exited(err)
	_ = conv.Close()
	_ = events.Close()
	if err := <-done; err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "otelgotest: process events: %v\n", err)
	}
	if err != nil {
		return 1
	}
	return 0
}

// killTimeouts returns go test's backstop for a wedged test binary: how long
// after starting it to signal it, and how long to then wait for its output.
// There is none without a timeout, or when benchmarking or fuzzing.
func killTimeouts(timeout time.Duration, benchOrFuzz bool) (kill, waitDelay time.Duration) {
	if timeout <= 0 || benchOrFuzz {
		return 0, 0
	}
	waitDelay = max(timeout/10, 5*time.Second)
	return timeout + max(waitDelay, time.Minute), waitDelay
}

// testBinaryPackage returns the import path of the package a test binary
// tests: its build info path without ".test", which is how oteltestctx in
// the binary names it when it asks for its test's span.
func testBinaryPackage(binary string, info *buildinfo.BuildInfo) string {
	if info != nil && info.Path != "" {
		return strings.TrimSuffix(info.Path, ".test")
	}
	pkg := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(binary), ".exe"), ".test")
	_, _ = fmt.Fprintf(os.Stderr, "otelgotest: cannot read the build info of %s; reporting package %q (set it with -p)\n", binary, pkg)
	return pkg
}

// builtWithCover reports whether a test binary was built with -cover, which
// is when go test reports its coverage on the ok line.
func builtWithCover(info *buildinfo.BuildInfo) bool {
	if info == nil {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-cover" {
			return s.Value == "true"
		}
	}
	return false
}

// fuzzCacheDir is where go test keeps a package's fuzzing corpus:
// $GOCACHE/fuzz/<import path>.
func fuzzCacheDir(pkg string) string {
	cache := os.Getenv("GOCACHE")
	if cache == "" || cache == "off" {
		if dir, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(dir, "go-build")
		} else {
			cache = filepath.Join(os.TempDir(), "go-build")
		}
	}
	return filepath.Join(cache, "fuzz", pkg)
}

// The testing package's boolean flags, which take no separate value.
var boolTestFlags = map[string]bool{
	"test.artifacts":    true,
	"test.benchmem":     true,
	"test.failfast":     true,
	"test.fullpath":     true,
	"test.fuzzworker":   true,
	"test.paniconexit0": true,
	"test.short":        true,
	"test.v":            true,
}

// lookupTestFlag returns the last value given for the test binary flag
// -test.<name>, in the -flag=value or -flag value form, scanning as the flag
// package parses: up to "--" or the first argument that is neither a flag
// nor a flag's value. The testing package's own flags take a value unless
// they are boolean; without the binary's flag set, any other flag given
// without "=" is assumed to take the next argument as its value unless that
// looks like a flag.
func lookupTestFlag(args []string, name string) (value string, ok bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		flagName, flagValue, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if !hasValue && !boolTestFlags[flagName] && i+1 < len(args) {
			// The flag package takes the next argument as a flag's value
			// whatever it looks like.
			if next := args[i+1]; strings.HasPrefix(flagName, "test.") || !strings.HasPrefix(next, "-") {
				i++
				flagValue, hasValue = next, true
			}
		}
		if flagName == "test."+name && hasValue {
			value, ok = flagValue, true
		}
	}
	return value, ok
}

// The warning a test binary prints when -test.run matches no test, which go
// test reports as "[no tests to run]".
var noTestsToRun = []byte("\ntesting: warning: no tests to run\n")

// What go test looks for in a test binary's output to report its coverage.
var coveragePrefix = []byte("coverage: ")

// The longest output line searched for coverage.
const maxCoverageLine = 4096

// outputTracker passes a test binary's output through, remembering what go
// test checks it for, without keeping it all.
type outputTracker struct {
	w       io.Writer
	n       int64
	last    byte
	tail    []byte
	noTests bool
	// coverage is what follows "coverage: " on the first line that has it.
	coverage string
	// line is the current line while it may still be searched for coverage.
	line     []byte
	longLine bool
}

func newOutputTracker(w io.Writer) *outputTracker {
	// The output's start counts as a line start.
	return &outputTracker{w: w, tail: []byte("\n")}
}

func (o *outputTracker) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	o.n += int64(len(b))
	o.last = b[len(b)-1]
	if !o.noTests {
		buf := append(o.tail, b...)
		o.noTests = bytes.Contains(buf, noTestsToRun)
		keep := min(len(buf), len(noTestsToRun)-1)
		o.tail = append(o.tail[:0:0], buf[len(buf)-keep:]...)
	}
	if o.coverage == "" {
		o.findCoverage(b)
	}
	return o.w.Write(b)
}

// findCoverage looks for go test's coverage line, `coverage: (.*)\n`.
func (o *outputTracker) findCoverage(b []byte) {
	for len(b) > 0 && o.coverage == "" {
		nl := bytes.IndexByte(b, '\n')
		chunk := b
		if nl >= 0 {
			chunk = b[:nl]
		}
		if !o.longLine {
			if len(o.line)+len(chunk) > maxCoverageLine {
				o.longLine = true
				o.line = o.line[:0]
			} else {
				o.line = append(o.line, chunk...)
			}
		}
		if nl < 0 {
			return
		}
		if !o.longLine {
			if i := bytes.Index(o.line, coveragePrefix); i >= 0 {
				o.coverage = string(o.line[i+len(coveragePrefix):])
			}
		}
		o.line = o.line[:0]
		o.longLine = false
		b = b[nl+1:]
	}
}
