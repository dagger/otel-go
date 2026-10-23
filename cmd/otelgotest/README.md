# otelgotest

The default mode wraps `go test` and forwards its flags.

## Prebuilt test batches

`otelgotest --test-binaries /suite/manifest.json -json` runs prebuilt test
binaries, with the same JSON events, test spans, and span-context socket as the
normal mode. Omit `-json` for readable output; add `-v` for passing test output.

The manifest contains `Packages`, `Args`, and optional `Parallel` (defaults to
GOMAXPROCS). Each package has `Package` (Go import path), `Binary` and `Dir`.
Paths are absolute or relative to the manifest. `Args` contains native testing
flags such as `-test.count=1` and `-test.run=^TestExample$`. The runner sets the
Go test default timeout of 10 minutes and then applies `Args`.

A package can instead have `Error` for a compilation failure, or `Skip: true`
when build constraints leave no tests. Failed packages do not stop the others.
The caller builds the binaries and runs vet if required. This mode does not
compile, cache test results, translate GOFLAGS, or merge coverage profiles.
It requires `go tool test2json` from a Go toolchain compatible with the binaries.

For options which require the Go command itself, a caller can instead provide
`GoTestArgs` and `Dir`. The runner executes ordinary `go test -json` in that
directory, with the same telemetry handling. This compatibility path does not
use prebuilt binaries.
