// Package prebuilt holds tests otelgotest runs both through go test and as a
// prebuilt test binary.
package prebuilt

import (
	"flag"
	"os"
	"testing"
)

var (
	greeting = flag.String("greeting", "", "a custom test flag, which must reach the binary")
	exitCode = flag.Int("exit", 0, "make TestExit exit with this status")
)

func TestPass(t *testing.T) {
	t.Log("passing output", Echo("x"))
}

func TestFail(t *testing.T) {
	t.Log("failing output")
	t.Error("expected failure")
}

func TestSkip(t *testing.T) {
	t.Skip("not today")
}

func TestSub(t *testing.T) {
	t.Run("child", func(t *testing.T) {
		t.Log("in child")
	})
}

// go test runs a test binary from its package's directory.
func TestWorkdir(t *testing.T) {
	if _, err := os.Stat("prebuilt_test.go"); err != nil {
		t.Fatal(err)
	}
}

func TestFlag(t *testing.T) {
	if *greeting != "hello" {
		t.Fatalf("-greeting=%q, want hello", *greeting)
	}
}

func TestExit(t *testing.T) {
	if *exitCode != 0 {
		os.Exit(*exitCode)
	}
}

func FuzzEcho(f *testing.F) {
	f.Add("a")
	f.Fuzz(func(t *testing.T, s string) {
		_ = Echo(s)
	})
}

// TestTimeout checks the binary has a timeout, as go test's default gives it.
func TestTimeout(t *testing.T) {
	if _, ok := t.Deadline(); !ok {
		t.Fatal("no timeout")
	}
}

// TestCoverDir checks a covered binary gets the coverage directory go test
// gives it, also as GOCOVERDIR for the covered programs tests run.
func TestCoverDir(t *testing.T) {
	dir := os.Getenv("GOCOVERDIR")
	if dir == "" {
		t.Fatal("GOCOVERDIR is not set")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	t.Log("GOCOVERDIR is set")
}
