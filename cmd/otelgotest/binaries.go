package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dagger/otel-go/gotest"
)

// binarySuite describes already compiled packages. Paths are absolute or
// relative to the manifest. Args are native testing flags (-test.run, etc.),
// not go command flags. The caller is responsible for compilation and vet.
// A build error belongs to its package and does not prevent other packages
// from running. Parallel defaults to GOMAXPROCS, like go test's -p.
type binarySuite struct {
	Packages []binaryPackage
	Args     []string
	Parallel int
	// GoTestArgs selects the ordinary go test path for options which require
	// go-command orchestration, such as fuzzing or merged coverage profiles.
	GoTestArgs []string
	Dir        string
}

type binaryPackage struct {
	Package string
	Binary  string
	Dir     string
	Error   string
	Skip    bool
}

func binaryMode(args []string) bool {
	return len(args) > 0 && args[0] == "--test-binaries"
}

// testCommand keeps the existing telemetry reader and span-context socket for
// both modes. The child multiplexes complete JSON records from the packages;
// there is still only one otelgotest invocation by the caller.
func testCommand(args []string) (*exec.Cmd, error) {
	if !binaryMode(args) {
		return exec.Command("go", buildGoTestArgs(args)...), nil
	}
	if len(args) < 2 {
		return nil, errors.New("--test-binaries requires a manifest path")
	}
	for _, arg := range args[2:] {
		if arg != "-json" && arg != "-v" {
			return nil, fmt.Errorf("unsupported binary-mode option %q; put native testing flags in the manifest", arg)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(self, "--run-test-binaries", args[1]), nil
}

// recordWriter serializes writes from test2json processes. os/exec may split
// its reads anywhere, so each child has a lineWriter to retain partial lines.
type recordWriter struct {
	sync.Mutex
	io.Writer
}

func (w *recordWriter) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	return w.Writer.Write(p)
}

func runBinarySuite(manifest string, stdout, stderr io.Writer) int {
	data, err := os.ReadFile(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var suite binarySuite
	if err := json.Unmarshal(data, &suite); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if suite.GoTestArgs != nil {
		cmd := exec.Command("go", append([]string{"test", "-json"}, suite.GoTestArgs...)...)
		cmd.Dir = suite.Dir
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
		return 0
	}
	if suite.Parallel < 0 {
		fmt.Fprintln(stderr, "Parallel must not be negative")
		return 1
	}
	if suite.Parallel == 0 {
		suite.Parallel = runtime.GOMAXPROCS(0)
	}
	base, err := filepath.Abs(filepath.Dir(manifest))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	seen := map[string]bool{}
	for _, pkg := range suite.Packages {
		if pkg.Package == "" || seen[pkg.Package] || (pkg.Binary == "" && pkg.Error == "" && !pkg.Skip) {
			fmt.Fprintln(stderr, "invalid or duplicate package in binary manifest")
			return 1
		}
		seen[pkg.Package] = true
	}
	// New Go toolchains build test2json on demand. Resolve it once before
	// starting workers: concurrent go-tool launches can write and execute the
	// same cached executable, causing ETXTBSY ("text file busy").
	var test2json string
	for _, pkg := range suite.Packages {
		if pkg.Error != "" || pkg.Skip {
			continue
		}
		cmd := exec.Command("go", "tool", "-n", "test2json")
		cmd.Stderr = stderr
		path, err := cmd.Output()
		if err != nil {
			fmt.Fprintln(stderr, "resolve test2json:", err)
			return 1
		}
		test2json = strings.TrimSpace(string(path))
		if test2json == "" {
			fmt.Fprintln(stderr, "go tool returned an empty test2json path")
			return 1
		}
		break
	}
	output := &recordWriter{Writer: stdout}
	diagnostics := &recordWriter{Writer: stderr}
	jobs := make(chan binaryPackage)
	results := make(chan int, len(suite.Packages))
	var wg sync.WaitGroup
	for range min(suite.Parallel, len(suite.Packages)) {
		wg.Go(func() {
			for pkg := range jobs {
				report := func(action, message string) {
					// Match go test's package events: omit Test, rather than
					// encode an empty test name. Some report consumers
					// distinguish a missing field from an empty string.
					event := map[string]any{"Time": time.Now(), "Action": action, "Package": pkg.Package}
					if message != "" {
						event["Output"] = message
					}
					_ = json.NewEncoder(output).Encode(event)
				}
				if pkg.Error != "" {
					report("start", "")
					report("output", pkg.Error+"\n")
					report("fail", "")
					results <- 1
					continue
				}
				if pkg.Skip {
					report("start", "")
					report("skip", "")
					results <- 0
					continue
				}
				// test2json supplies package identity, subtests, failures and elapsed
				// times. test.v=test2json prevents ordinary test output from forging
				// testing control records. Go's default timeout is not the binary's.
				args := []string{"-t", "-p", pkg.Package, resolve(pkg.Binary), "-test.timeout=10m"}
				args = append(args, suite.Args...)
				args = append(args, "-test.v=test2json")
				cmd := exec.Command(test2json, args...)
				cmd.Dir = resolve(pkg.Dir)
				packageFinished := false
				lines := &lineWriter{out: output, observe: func(line []byte) {
					var event gotest.TestEvent
					if json.Unmarshal(line, &event) == nil && event.Test == "" && (event.Action == "pass" || event.Action == "fail" || event.Action == "skip") {
						packageFinished = true
					}
				}}
				cmd.Stdout = lines
				cmd.Stderr = diagnostics
				err := cmd.Run()
				lines.flush()
				if err != nil {
					// test2json emits fail for an unsuccessful binary. A launch error has
					// no JSON stream, so provide the missing package result ourselves.
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						report("start", "")
						report("output", err.Error()+"\n")
						report("fail", "")
						packageFinished = true
					}
					if !packageFinished {
						report("fail", "")
					}
					results <- 1
				} else {
					results <- 0
				}
			}
		})
	}
	for _, pkg := range suite.Packages {
		jobs <- pkg
	}
	close(jobs)
	wg.Wait()
	close(results)
	code := 0
	for result := range results {
		if result != 0 {
			code = 1
		}
	}
	return code
}

// At most one incomplete record per package is retained. Completed records
// are forwarded immediately; output does not grow with the number of tests.
type lineWriter struct {
	out     io.Writer
	pending []byte
	observe func([]byte)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := 0
		for i < len(p) && p[i] != '\n' {
			i++
		}
		if i == len(p) {
			w.pending = append(w.pending, p...)
			break
		}
		w.pending = append(w.pending, p[:i+1]...)
		if w.observe != nil {
			w.observe(w.pending)
		}
		if _, err := w.out.Write(w.pending); err != nil {
			return 0, err
		}
		w.pending = w.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}
func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		_, _ = w.out.Write(w.pending)
		w.pending = nil
	}
}
