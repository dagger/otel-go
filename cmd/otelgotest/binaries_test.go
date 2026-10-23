package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagger/otel-go/gotest"
)

// Build real test binaries, then check the same JSON contract as go test:
// package identity, package cwd/testdata, filters, subtests, failure and skip.
func TestBinarySuite(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/suite\n\ngo 1.25.0\n")
	for _, pkg := range []string{"a", "b"} {
		write(pkg+"/testdata/value", "fixture")
		write(pkg+"/x_test.go", `package suite
import("testing";"os")
func TestFixture(t *testing.T){b,e:=os.ReadFile("testdata/value");if e!=nil||string(b)!="fixture"{t.Fatal(string(b),e)};t.Run("child",func(t *testing.T){t.Log("child")})}
func TestSkip(t *testing.T){t.Skip("skip")}
func TestFailure(t *testing.T){t.Fatal("failure detail")}
func TestPanic(t *testing.T){panic("panic detail")}
`)
		cmd := exec.Command("go", "test", "-c", "-o", filepath.Join(root, pkg+".test"), "./"+pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, output)
		}
	}
	for _, tc := range []struct {
		name, pattern string
		code          int
	}{
		{"pass", "TestFixture|TestSkip", 0}, {"failure", "TestFixture|TestFailure", 1}, {"panic", "TestPanic", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := binarySuite{Parallel: 2, Args: []string{"-test.run=" + tc.pattern, "-test.count=1"}}
			for _, pkg := range []string{"a", "b"} {
				suite.Packages = append(suite.Packages, binaryPackage{Package: "example.com/suite/" + pkg, Binary: pkg + ".test", Dir: pkg})
			}
			data, _ := json.Marshal(suite)
			write("manifest.json", string(data))
			var out, errout bytes.Buffer
			if code := runBinarySuite(filepath.Join(root, "manifest.json"), &out, &errout); code != tc.code {
				t.Fatalf("code %d, want %d: %s\n%s", code, tc.code, out.String(), errout.String())
			}
			events := map[string]map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var event gotest.TestEvent
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatalf("corrupt event: %v: %q", err, line)
				}
				if events[event.Package] == nil {
					events[event.Package] = map[string]string{}
				}
				if event.Action == "pass" || event.Action == "fail" || event.Action == "skip" {
					events[event.Package][event.Test] = event.Action
				}
			}
			for _, pkg := range []string{"a", "b"} {
				got := events["example.com/suite/"+pkg]
				want := "pass"
				if tc.code != 0 {
					want = "fail"
				}
				if got[""] != want {
					t.Fatalf("package result: %v", got)
				}
				if tc.name == "pass" && (got["TestFixture/child"] != "pass" || got["TestSkip"] != "skip" || got["TestFailure"] != "") {
					t.Fatalf("filter/subtest/skip: %v", got)
				}
			}
		})
	}
	t.Run("go test compatibility", func(t *testing.T) {
		suite := binarySuite{GoTestArgs: []string{"-run=TestFixture", "-count=1", "./a", "./b"}, Dir: root}
		data, _ := json.Marshal(suite)
		write("manifest.json", string(data))
		var out, errout bytes.Buffer
		t.Setenv("GOWORK", "off")
		t.Setenv("GOFLAGS", "")
		if code := runBinarySuite(filepath.Join(root, "manifest.json"), &out, &errout); code != 0 {
			t.Fatalf("exit %d: %s", code, errout.String())
		}
		for _, pkg := range []string{"a", "b"} {
			if !strings.Contains(out.String(), `"Action":"pass","Package":"example.com/suite/`+pkg+`"`) {
				t.Fatal(out.String())
			}
		}
	})
	t.Run("package skip omits Test", func(t *testing.T) {
		suite := binarySuite{Packages: []binaryPackage{{Package: "tagged", Skip: true}}}
		data, _ := json.Marshal(suite)
		write("manifest.json", string(data))
		var out, errout bytes.Buffer
		if code := runBinarySuite(filepath.Join(root, "manifest.json"), &out, &errout); code != 0 {
			t.Fatal(code)
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if _, ok := event["Test"]; ok {
				t.Fatalf("package event has Test: %s", line)
			}
		}
	})
	t.Run("build error isolation", func(t *testing.T) {
		suite := binarySuite{Packages: []binaryPackage{{Package: "broken", Error: "compile error"}, {Package: "good", Binary: "a.test", Dir: "a"}}, Args: []string{"-test.run=TestFixture"}}
		data, _ := json.Marshal(suite)
		write("manifest.json", string(data))
		var out, errout bytes.Buffer
		if code := runBinarySuite(filepath.Join(root, "manifest.json"), &out, &errout); code != 1 {
			t.Fatalf("exit: %d", code)
		}
		if !strings.Contains(out.String(), `"Action":"pass","Package":"good"`) || !strings.Contains(out.String(), "compile error") {
			t.Fatal(out.String())
		}
	})
}

func TestLineWriter(t *testing.T) {
	var out bytes.Buffer
	w := lineWriter{out: &out}
	for _, part := range []string{"{\"a", "\":1}\n{", "\"b\":2}", "\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := out.String(), "{\"a\":1}\n{\"b\":2}\n"; got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
