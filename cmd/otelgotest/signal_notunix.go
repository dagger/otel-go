//go:build !(unix || js || wasip1)

package main

import "os"

// signalTrace is the signal go test sends a wedged test binary so it prints
// its goroutines before exiting; without one, it kills the binary.
var signalTrace os.Signal
