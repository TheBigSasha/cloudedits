# Usage Guide

`cloudedits` ships as a single executable supporting three workflows: Desktop Setup GUI, Headless CLI, and Docker/NAS.

**Tailscale networking model:** Tailscale is required for remote access and runs **at the library server level** via embedded `tailscale.com/tsnet` using a pre-approved auth token — **not** system-wide on the host machine. Once enrolled, your library is reachable from your tailnet at its **Tailscale MagicDNS URL (`https://library.<tailnet>.ts.net`)** and **Tailscale IP (`100.x.y.z`)**.

## Mode 1: Desktop Setup GUI

```bash
cloudedits gui
```

Opens a loopback setup wizard in your default browser:
1. Choose one or more photo folders (source files are strictly read-only; edits save under `.hdrtoys/`).
2. Choose your Tailscale hostname (default `library` -> MagicDNS `https://library.<tailnet>.ts.net` and Tailscale IP `100.x.y.z`) and paste a pre-approved one-time auth token from `https://login.tailscale.com/admin/settings/keys`.
3. Verify MagicDNS + HTTPS readiness (if disabled, enable both at `https://login.tailscale.com/admin/dns`).
4. Scan the QR code from your phone or click **Open in hdr.toys**.

## Mode 2: Headless CLI (Automation & Agents)

Every CLI subcommand supports `--json` and deterministic exit codes:
- `0`: success
- `2`: invalid arguments / usage
- `3`: user action required (for example, Tailscale HTTPS certificates not yet enabled)
- `4`: storage or filesystem error

### Commands

```bash
# Configure folders and Tailscale hostname (reads auth key from stdin)
echo "$TS_AUTHKEY" | cloudedits setup \
  --folder ~/Pictures \
  --hostname library \
  --tailscale-auth-key-stdin \
  --json

# Run foreground server on Tailscale HTTPS (:443)
cloudedits serve --tsnet

# Run foreground server on local HTTP (loopback/LAN)
cloudedits serve --listen 127.0.0.1:8787 --folder ~/Pictures

# Require explicit bearer token (read from the environment; flags are visible in the process list)
CLOUDEDITS_TOKEN="$TOKEN" cloudedits serve --listen 127.0.0.1:8787 --folder ~/Pictures --require-token

# Allow an additional editor origin (repeatable; https://hdr.toys is always allowed)
cloudedits serve --tsnet --allow-origin https://preview.example.com

# Check status and run diagnostics
cloudedits status --json
cloudedits doctor --json

# Manage configured photo folders
cloudedits folders list --json
cloudedits folders add ~/Photos/Trips --json
cloudedits folders remove ~/Photos/Trips --json

# Print terminal QR code (optionally deep-linking an asset)
cloudedits qr
cloudedits qr --asset <assetId> --json

# Generate Docker Compose bundle
cloudedits docker-init --folder /volume1/photos --out ./cloudedits-docker --json
```

## Mode 3: Docker / NAS

`cloudedits` embeds userspace `tsnet` and does **not** require `NET_ADMIN`, `/dev/net/tun`, or root privileges.

```yaml
services:
  cloudedits:
    image: ghcr.io/thebigsasha/cloudedits:latest
    restart: unless-stopped
    environment:
      CLOUDEDITS_HOSTNAME: library
      TS_AUTHKEY: ${TS_AUTHKEY}
    volumes:
      - /volume1/photos:/library/photos
      - cloudedits_data:/data

volumes:
  cloudedits_data:
```

If `/library/photos` must be mounted `:ro`, add `CLOUDEDITS_SIDECAR_DIR: /data/sidecars` (or `--sidecar-dir /data/sidecars`).

## Tailscale Troubleshooting & Security Posture

- **Cannot reach `https://library.<tailnet>.ts.net`?** Confirm Tailscale is connected on both host and client devices, both belong to the same tailnet (or the node is explicitly shared), and MagicDNS + HTTPS Certificates are enabled at `https://login.tailscale.com/admin/dns`.
- **How secure is this?** We try to keep it secure, but it's only as secure as your Tailscale setup and is written using AI tools, so use it with caution. Securing your tailnet, ACLs, and bearer tokens is your responsibility.
