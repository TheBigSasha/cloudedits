# Security Policy

## Security Posture

We try to keep cloudedits secure, but it is only as secure as your Tailscale setup and is written using AI tools, so use it with caution.

`cloudedits` is designed to expose your local photo library over your Tailscale network (`tailscale.com/tsnet`) or an authenticated loopback/LAN endpoint.

- **Tailnet access boundary:** When running in default Tailscale mode (`--auth=tailnet`), access is enforced by cryptographically verified Tailscale peer identity (`WhoIs`) and scoped to devices belonging to your tailnet owner account unless explicitly opened or switched to `--require-token`.
- **No secrets in URLs or QR codes:** Shareable URLs (`https://library.<tailnet>.ts.net`, `/open?a=...`, `/h/<id>`) never embed long-lived bearer tokens. Browsers exchange Tailnet identity via `POST /auth/session` for short-lived, memory-only session tokens and signed `?bt=` blob tokens.
- **No warranty on network configuration:** Your photo library is as secure as your Tailscale ACLs, device security, and token hygiene. Anyone you grant network access to can read assets permitted by your configuration.
- **Read-only source files:** `cloudedits` never modifies source media files and guards all file resolution against directory traversal (`..`, symlink escapes outside configured roots).
- **Compile-time dev route elimination:** `/__dev/*` test endpoints are gated by `//go:build dev_apis` and are completely absent from release binaries.

## Reporting a Vulnerability

Please report security issues privately by emailing `security@sashaisalex.com` or opening a private GitHub Security Advisory on the repository. Do not open public issues for unpatched vulnerabilities.
