//go:build unix || js || wasip1

package main

import (
	"os"
	"syscall"
)

// signalTrace is the signal go test sends a wedged test binary so it prints
// its goroutines before exiting.
var signalTrace os.Signal = syscall.SIGQUIT
