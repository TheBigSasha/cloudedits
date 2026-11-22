# cloudedits

Single-binary remote photo library server for [hdr.toys](https://hdr.toys).

Serve your local photo folders over your private Tailscale network (`https://library.<tailnet>.ts.net`) or loopback/LAN, keep source files strictly read-only, and persist all edits, ratings, flags, labels, keywords, variants, and edited thumbnails in portable `.hdrtoys/<rel-path>.edits` sidecars.

## Core Principles

- **Source files are strictly read-only.** `cloudedits` never modifies, renames, or deletes original photos. All mutations write exclusively inside `<folder>/.hdrtoys/` (or an explicit `--sidecar-dir` for `:ro` mounts).
- **100% on-disk intercompatibility.** Sidecars use the canonical `.hdrtoys/<rel-posix-path>.edits` (`hdrt: 1`) HLC format so opening the same folder locally in `hdr.toys` or remotely through `cloudedits` shares every edit, rating, keyword, variant, and preview thumbnail with zero migration.
- **Rebuildable index.** The SQLite WAL index and thumbnail cache live outside your photo folders. Deleting them and restarting reconstructs library state losslessly from source files and `.hdrtoys/`.
- **Dedicated Tailscale identity (`tsnet`).** Embeds userspace `tailscale.com/tsnet` with automatic Let's Encrypt HTTPS certificates (`ListenTLS(":443")`), zero root privileges, and no `NET_ADMIN` or `/dev/net/tun` required in Docker.
- **Fail-closed routing & compile-time dev isolation.** Every HTTP route declares its `AuthPolicy`, `Permission`, `ResourceScope`, and latency budget. Test harness routes under `/__dev/*` are compiled out of release binaries via `//go:build dev_apis`.

## Quick Start

### 1. Local / Loopback

```bash
go build -o cloudedits ./cmd/cloudedits
./cloudedits serve --listen 127.0.0.1:8787 --folder ~/Pictures
```

### 2. Dedicated Tailscale Node (`https://library.<tailnet>.ts.net`)

```bash
echo "$TS_AUTHKEY" | ./cloudedits setup \
  --folder ~/Pictures \
  --hostname library \
  --tailscale-auth-key-stdin \
  --json

./cloudedits serve --tsnet
./cloudedits qr
```

Opening `https://library.<tailnet>.ts.net` in a browser on your tailnet redirects straight into `https://hdr.toys` with your library connected.

### 3. Setup Wizard GUI

```bash
./cloudedits gui
```

Opens a loopback setup wizard for selecting photo folders, entering a one-time Tailscale auth key, checking MagicDNS/HTTPS readiness, and scanning the mobile QR code.

### 4. Docker / NAS

```bash
./cloudedits docker-init --folder /volume1/photos --out ./cloudedits-docker
cd cloudedits-docker && docker compose up -d
```

## Documentation

- [Usage & Troubleshooting](docs/USAGE.md)
- [Remote Library HTTP API v1 Contract](docs/EDITOR_CONTRACT.md)
- [OpenAPI 3.1 Specification](docs/openapi.yaml)
- [Sidecar `.edits` Format Specification](docs/SIDECAR_FORMAT.md)
- [Serving SLOs & Benchmarks](docs/SLOS.md)
- [Implementer Gotchas](docs/GOTCHAS.md)
- [Agent & Contributor Guide](AGENTS.md)
- [Security Policy](SECURITY.md)

## License

MIT — see [LICENSE](LICENSE).
