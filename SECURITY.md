# Security Policy

## Security Posture

We try to keep cloudedits secure, but it is only as secure as your Tailscale setup and is written using AI tools, so use it with caution.

`cloudedits` is designed to expose your local photo library over your Tailscale network (`tailscale.com/tsnet`) or an authenticated loopback/LAN endpoint.

- **Tailnet access boundary:** When running in Tailscale mode (`serve --tsnet`), access is enforced by cryptographically verified Tailscale peer identity (`WhoIs`) and scoped to devices belonging to your tailnet owner account unless explicitly opened or switched to `--require-token`.
- **No secrets in URLs or QR codes:** Shareable URLs (`https://library.<tailnet>.ts.net`, `/open?a=...`, `/h/<id>`) never embed bearer tokens or query-string credentials. Browsers exchange Tailnet identity via `POST /auth/session` for short-lived, memory-only session bearer tokens sent strictly in `Authorization: Bearer` headers.
- **Browser origin allowlist:** Tailnet and loopback identity are attached by the browser to every request, so only pages from an allowed origin may call the API: the hdr.toys editor, the server's own URL, origins added with `--allow-origin`, and locally served pages talking to a loopback listener. Any other `Origin` is refused with `403` before authentication.
- **Loopback trust is local only:** Requests are trusted as the local owner only on a direct loopback connection addressed to `localhost` or a loopback IP. DNS-rebound hostnames and requests relayed by a local reverse proxy or tunnel are not trusted and need a bearer token. The setup wizard (`cloudedits gui`) accepts only same-origin requests from the local machine.
- **Bounded thumbnail decoding:** Source images whose declared dimensions exceed the decode budget (192 MiB default, aligned with `GOMEMLIMIT`) are not thumbnailed, and failed decodes are negative-cached for a bounded window.
- **No warranty on network configuration:** Your photo library is as secure as your Tailscale ACLs, device security, and token hygiene. Anyone you grant network access to can read assets permitted by your configuration.
- **Read-only source files:** `cloudedits` never modifies source media files and guards all file resolution against directory traversal (`..`, symlink escapes outside configured roots).
- **Untrusted folder contents:** Sidecars, edited-preview thumbnails and auxiliary channels are read only from regular files within fixed size limits (16 MiB per `.edits` document and per auxiliary channel, 32 MiB per edited preview) and written through Go `os.Root` handles confined to the sidecar root. A symbolic link, device, FIFO or oversized file found at one of those names is never followed, read, or traversed to escape the root, and symbolic links are never indexed as photos.
- **Compile-time dev route elimination:** `/__dev/*` test endpoints are gated by `//go:build dev_apis` and are completely absent from release binaries. A `dev_apis` test build refuses to listen beyond loopback unless started with `--allow-dev-apis-on-network`.

## Reporting a Vulnerability

Please report security issues privately by emailing `security@sashaisalex.com` or opening a private GitHub Security Advisory on the repository. Do not open public issues for unpatched vulnerabilities.
