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
	t.Log("passing output")
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
