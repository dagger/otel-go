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
//	otelgotest [-json] path/to/pkg.test [test binary flags]
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
}

// go test's default -timeout, which the test binary itself does not have.
const defaultTestTimeout = 10 * time.Minute

// parsePrebuilt reports whether args name a prebuilt test binary rather than
// go test arguments: an existing regular file that is not a .go file,
// optionally after -json. No valid go test command line has that shape.
func parsePrebuilt(args []string) (*prebuilt, bool) {
	p := &prebuilt{}
	i := 0
	for ; i < len(args); i++ {
		if args[i] == "-json" || args[i] == "-json=true" {
			p.json = true
		} else if args[i] == "-json=false" {
			p.json = false
		} else {
			break
		}
	}
	if i == len(args) {
		return nil, false
	}
	bin := args[i]
	if strings.HasPrefix(bin, "-") || strings.HasSuffix(bin, ".go") {
		return nil, false
	}
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	p.binary = bin
	p.args = args[i+1:]
	return p, true
}

// runPrebuilt runs a prebuilt test binary as go test -json runs a test
// binary (see runTestActor.Act in cmd/go/internal/test), converts its output
// with the same converter, and processes the events with gotest.Run. env is
// added to the binary's environment. It returns go test's exit status.
func runPrebuilt(ctx context.Context, p *prebuilt, tp trace.TracerProvider, opts []gotest.Option, env []string, stdout io.Writer) int {
	pkg := testBinaryPackage(p.binary)
	userArgs := p.args

	// go test passes -test.paniconexit0, then -test.v=test2json and its
	// default timeout, then the user's flags. Like go test -json -v, a
	// user's -test.v overrides -test.v=test2json; test2json copes.
	args := []string{"-test.paniconexit0", "-test.v=test2json"}
	timeout := defaultTestTimeout
	if v, ok := lookupTestFlag(userArgs, "timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "otelgotest: invalid -test.timeout %q: %v\n", v, err)
			return 2
		}
		timeout = d
	} else {
		args = append(args, fmt.Sprintf("-test.timeout=%v", timeout))
	}
	args = append(args, userArgs...)
	bench, _ := lookupTestFlag(userArgs, "bench")
	fuzz, _ := lookupTestFlag(userArgs, "fuzz")
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
		// Keep the converter from blocking if Run stopped early.
		_, _ = io.Copy(io.Discard, r)
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
		if out.n > 0 && out.last != '\n' {
			_, _ = out.Write([]byte("\n"))
		}
		_, _ = fmt.Fprintf(out, "ok  \t%s\t%s%s\n", pkg, elapsed, norun)
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
// tests: its build info path without ".test", as oteltestctx reports it.
func testBinaryPackage(binary string) string {
	if bi, err := buildinfo.ReadFile(binary); err == nil && bi.Path != "" {
		return strings.TrimSuffix(bi.Path, ".test")
	}
	return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(binary), ".exe"), ".test")
}

// lookupTestFlag returns the last value given for the test binary flag
// -test.<name>, in the -flag=value or -flag value form.
func lookupTestFlag(args []string, name string) (value string, ok bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flagName, flagValue, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if flagName != "test."+name {
			continue
		}
		if !hasValue {
			if i+1 == len(args) {
				break
			}
			i++
			flagValue = args[i]
		}
		value, ok = flagValue, true
	}
	return value, ok
}

// The warning a test binary prints when -test.run matches no test, which go
// test reports as "[no tests to run]".
var noTestsToRun = []byte("\ntesting: warning: no tests to run\n")

// outputTracker passes a test binary's output through, remembering what go
// test checks it for, without keeping it all.
type outputTracker struct {
	w       io.Writer
	n       int64
	last    byte
	tail    []byte
	noTests bool
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
	return o.w.Write(b)
}
