# test2json

An unmodified copy of Go's `cmd/internal/test2json` (from Go 1.26.10), the
converter `go test -json` uses in-process. `cmd/otelgotest` uses it to run a
prebuilt test binary exactly as `go test -json` would, without needing a Go
toolchain at run time. `LICENSE` is Go's.

To update, copy `test2json.go`, `test2json_test.go` and `testdata/` from
`$(go env GOROOT)/src/cmd/internal/test2json` unchanged.
