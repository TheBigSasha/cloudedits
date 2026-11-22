# Contributing to cloudedits

Thank you for contributing to `cloudedits`.

## Development Setup

Requirements:

- Go 1.24 or newer
- `golangci-lint` v2 (optional locally, enforced in CI)

```bash
git clone https://github.com/thebigsasha/cloudedits.git
cd cloudedits
go test -race ./...
```

## Pull Request Checklist

1. Run `./scripts/check-public-boundary.sh` and ensure it exits `0`.
2. Run `go vet ./...` and `go vet -tags dev_apis ./...`.
3. Run `go test -race -count=1 ./...` and `go test -race -count=1 -tags dev_apis ./...`.
4. Run `golangci-lint run`.
5. Keep any API change in sync with `docs/openapi.yaml` and `docs/EDITOR_CONTRACT.md`.
6. Keep any `.edits` sidecar change in sync with `docs/SIDECAR_FORMAT.md` and `testdata/golden_edits/`.
7. Only add dependencies with permissive licenses (MIT, BSD, ISC, Apache-2.0).

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
