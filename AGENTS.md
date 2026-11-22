# Agent & Contributor Guide

This repository contains `cloudedits`, a standalone MIT-licensed Go server implementing the public **hdr.toys Remote Library HTTP API v1** and the canonical **`.hdrtoys/<rel-posix-path>.edits`** sidecar format.

## Non-Negotiable Public Repository Boundary

1. **Contract-only scope:** This repository is public and MIT-licensed. Document and implement only the public Remote Library HTTP API v1, the `.hdrtoys/` + `.edits` sidecar format, OpenAPI 3.1 contract, public SLOs, generic `//go:build dev_apis` test helpers, and standard browser/HTTP/storage engineering rules.
2. **No private internal references:** Never commit internal editor repository names, private paths, private issue numbers, private commit SHAs, or private test harness names.
3. **No dev logs:** Never create a `dev_logs/` directory or `dev-log` markdown files in this repository. Keep notes in PR descriptions, commit messages, or design docs under `docs/`.
4. **Automated gate:** Run `./scripts/check-public-boundary.sh` before every commit. CI enforces it on every push and pull request.
5. **Permissive dependencies only:** Only MIT, BSD, ISC, or Apache-2.0 Go modules may be added to `go.mod`. Never add GPL/AGPL dependencies.

## Engineering Invariants

- **Source folders are strictly read-only.** Never modify, rename, or remove original media files. Sidecars write only under `<folder>/.hdrtoys/` (or `--sidecar-dir` when configured).
- **Atomic durable sidecars.** Every `.edits` write acquires a per-asset lock, reads and merges disk state first, writes to `<sidecarRoot>/.tmp/<uuid>`, calls `fsync`, closes, and atomically renames into place.
- **Canonical `.edits` serialization.** Always emit `hdrt` first, recursively sort all object keys lexicographically, sort `variants` by `id`, use 2-space indentation, and end with `\n`. Preserve unknown top-level and nested fields verbatim.
- **Numeric HLC comparison.** Parse `<unixMillis-hex>-<counter-hex>-<deviceId>` (splitting on the first two `-` only) and compare numerically on `(millis, counter)` before `deviceId`. Never use raw string comparison on HLC values.
- **Fail-closed `RouteSpec` registry.** Every HTTP endpoint must register through `internal/api/routespec.go` with explicit `AuthPolicy`, `Permission` (when `Authenticated`), `ResourceScope`, and `SLOBudgetMs`.
- **Compile-time dev API isolation.** All `/__dev/*` handlers live strictly behind `//go:build dev_apis`. Default release builds compile only a zero-symbol stub (`dev_stub.go`).

## Verification Commands

```bash
./scripts/check-public-boundary.sh
go vet ./...
go vet -tags dev_apis ./...
go test -race -count=1 ./...
go test -race -count=1 -tags dev_apis ./...
golangci-lint run
```

To verify zero dev symbols in a release binary:

```bash
go build -trimpath -ldflags="-s -w" -o /tmp/cloudedits-release ./cmd/cloudedits
go tool nm /tmp/cloudedits-release | grep -E 'devapi|registerDevRoutesImpl|handleDev(Reset|Fault|Stats)' && exit 1 || echo OK
rm -f /tmp/cloudedits-release
```
