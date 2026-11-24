---
name: otel-bump
description: Use when bumping OpenTelemetry (go.opentelemetry.io/otel) in dagger/otel-go, tagging an otel-go release, or propagating an otel-go bump to its dependents (testctx, go-sdk, dagger/dagger, dagger/go, alpine, dagger.io).
---

# Bumping OpenTelemetry in otel-go

## 1. Versioning policy

- otel-go tags mirror upstream go.opentelemetry.io/otel releases 1:1: otel
  `vX.Y.Z` → otel-go `vX.Y.Z`. Never tag a version OTel doesn't have.
- Between OTel releases, dependents take otel-go pseudo-versions, not new
  tags: `go get github.com/dagger/otel-go@<sha>` →
  `vX.Y.(Z+1)-0.<timestamp>-<sha>`.
- Tags are immutable once fetched. proxy.golang.org and sum.golang.org cache
  them, so moving or re-cutting a tag causes checksum errors. A pre-release
  suffix sorts below the release and build metadata (`+foo`) is rejected, so
  neither fixes a bad tag. Finish all of section 2 before tagging.
- If a tag ships broken anyway: leave it where it is and land the fix on
  main. Until the next tag, point dependents (and dagger/go's
  `testRunnerCommit`) at the fix commit's pseudo-version, not the tag.

## 2. Bump OTel in otel-go (before tagging)

1. Bump every `go.opentelemetry.io/otel/...` module to the same release:
   otel, trace, metric, sdk, sdk/metric, log, sdk/log and the OTLP
   exporters. Modules upstream still ships pre-1.0 (e.g. the OTLP log
   exporters, `v0.23.0` alongside `v1.47.0`) take the 0.x version released
   with it. Then `go mod tidy`, and check the `go` directive.
2. Find the semconv version upstream's SDK uses at that release:
   ```sh
   grep -rho 'semconv/v1\.[0-9.]*' "$(go list -m -f '{{.Dir}}' go.opentelemetry.io/otel/sdk)/resource" | sort -u
   ```
3. Move EVERY semconv import in otel-go to that version:
   `grep -rn 'semconv/v1\.' --include='*.go' .` must show only that version.
   `TestFallbackResourceMergesWithDefault` (init_test.go) must pass, since
   the fallback resource's SchemaURL must equal `resource.Default()`'s.
4. Compare every semconv constant otel-go uses between the old and the new
   version (`go doc go.opentelemetry.io/otel/semconv/vA.B.C <Name>`, or a
   throwaway program that compares them with `==`). Keep emitted keys and
   values identical (test.*, service.*, etc. are read by Dagger's UI and
   Cloud), or call out any deliberate rename in the PR.
5. Read upstream's CHANGELOG for every release you skip over, and carry API
   breaks and deprecations through.
6. Run `go build ./...`, `go vet ./...` and `go test ./...` in every module
   (`find . -name go.mod -not -path '*/testdata/*'`). The testdata modules
   are fixtures whose tests fail on purpose. Merge, then tag the merge
   commit.

## 3. Propagate to dependents (in this order)

- Find them:
  - `gh search code github.com/dagger/otel-go --owner dagger --filename go.mod`
  - `gh search code semconv --owner dagger`
  - Skip read-only mirrors (dagger-go-sdk, dagger-php-sdk). Leave testdata
    fixtures that pin older versions on purpose as they are.
- Everywhere, a program's semconv must match the version its OTel SDK's
  resource detectors use. Otherwise resource merges fail with
  "conflicting Schema URL".
- **dagger/testctx**: bump the nested `oteltest/` module. After merge, tag
  it `oteltest/vX.Y.Z`.
- **dagger/go-sdk**: the generated-code templates (`module.go.tmpl`,
  `templates/v2.go`, `templates/modules.go`, `generated-imports.txt`)
  import semconv. Keep them on otel-go's semconv version. Never hand-edit
  generated `dagger.gen.go` / `dagger.dispatch.gen.go` elsewhere: move them
  by bumping the go-sdk pin and regenerating.
- **dagger/dagger**:
  - Bump the root go.mod, `sdk/go`, `.dagger/modules/{cli-dev,engine-dev}`
    and `dagql/idtui/viztest`.
  - Make `cmd/codegen`'s Go module template and `sdk/go/internal/codegendeps`
    import otel-go's semconv. Otherwise the prebuilt archives lack the
    package and prepare-go-runtime fails with "export driver declined to
    serve codegen from the prebuilt archives".
  - CI's `go-sdk/generate/stale` runs on the RELEASED engine, so legacy
    `runtime=go` modules (viztest) may have to lag until a release includes
    the bump.
  - The CLI resource's semconv must match the detectors'. Otherwise every
    command warns "conflicting Schema URL".
- **dagger/go**: bump `gobuild/go.mod`, and set `testRunnerCommit` in
  `go.dang` (it builds otelgotest from an otel-go commit) to the tag's
  commit, or to the fix commit if the tag is broken (see section 1).
- **dagger/alpine, dagger/dagger.io**: bump their many modules. dagger.io's
  engine-runtime `.dagger` modules, and anything importing dagger/dagger
  engine packages, wait until a dagger release includes the bump.
- otel-go ≥ v1.47 exports only http/protobuf unless a program imports
  `github.com/dagger/otel-go/otlp`. Programs that send to gRPC collectors
  must import it.
