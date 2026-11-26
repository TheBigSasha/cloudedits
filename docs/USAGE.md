# Usage Guide

`cloudedits` ships as a single executable supporting three workflows: Desktop Setup GUI, Headless CLI, and Docker/NAS.

**Tailscale networking model:** Tailscale is required for remote access and runs **at the library server level** via embedded `tailscale.com/tsnet` using a pre-approved auth token — **not** system-wide on the host machine. Once enrolled, your library is reachable from your tailnet at its **Tailscale MagicDNS URL (`https://library.<tailnet>.ts.net`)** and **Tailscale IP (`100.x.y.z`)**.

## Mode 1: Desktop Setup GUI

```bash
cloudedits gui
```

Opens a loopback setup wizard in your default browser:
1. Add one or more photo folders by path (source files are strictly read-only; edits save under `.hdrtoys/`).
2. Choose your Tailscale hostname (default `library` -> MagicDNS `https://library.<tailnet>.ts.net` and Tailscale IP `100.x.y.z`). The wizard links to `https://login.tailscale.com/admin/settings/keys` for a pre-approved one-time auth key and to `https://login.tailscale.com/admin/dns` for MagicDNS + HTTPS, then enrols the node directly and installs the per-user background service.
3. Once the node is enrolled and serving, the wizard shows the library's tailnet URL, its QR code, MagicDNS/HTTPS self-check status, and **Open in hdr.toys**; until then it shows none of them, because no other device can reach the library yet.

## Mode 2: Headless CLI (Automation & Agents)

Every CLI subcommand supports `--json` and deterministic exit codes:
- `0`: success
- `2`: invalid arguments / usage
- `3`: user action required (for example, Tailscale HTTPS certificates not yet enabled)
- `4`: storage or filesystem error

### Commands

```bash
# Configure folders, enrol on Tailscale (reads auth key from stdin), and install the login service
echo "$TS_AUTHKEY" | cloudedits setup \
  --folder ~/Pictures \
  --hostname library \
  --tailscale-auth-key-stdin \
  --install-service \
  --json

# Manage the per-user background service (launchd / systemd --user / Task Scheduler)
cloudedits service install --json
cloudedits service status --json
cloudedits service uninstall --json

# Run foreground server on Tailscale HTTPS (:443)
cloudedits serve --tsnet

# If enrolling with a tagged Tailscale auth key, pin your numeric Tailscale UserID
# (tagged nodes fail closed unless --owner-user-id / CLOUDEDITS_OWNER_USER_ID or --require-token is set)
cloudedits serve --tsnet --owner-user-id 1234567890

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
    user: "${PUID:-10001}:${PGID:-10001}"
    environment:
      CLOUDEDITS_HOSTNAME: library
      TS_AUTHKEY: ${TS_AUTHKEY}
    volumes:
      - /volume1/photos:/library/photos
      - cloudedits_data:/data

volumes:
  cloudedits_data:
```

The image's default command is `serve --tsnet --folder /library/photos`, so the mount above needs no `command:`. Set `PUID` and `PGID` to the owner of `/volume1/photos` so the container can create `/library/photos/.hdrtoys/` without changing host permissions (`/data` and `/data/sidecars` are writable by any mapped non-root UID/GID).

If `/library/photos` must be mounted `:ro`, or cannot be written by the container user, add `CLOUDEDITS_SIDECAR_DIR: /data/sidecars` (or `--sidecar-dir /data/sidecars`). If `TS_AUTHKEY` creates a tagged Tailscale node, also set `CLOUDEDITS_OWNER_USER_ID` (tagged nodes fail closed by default).

## Tailscale Troubleshooting & Security Posture

- **Cannot reach `https://library.<tailnet>.ts.net`?** Confirm Tailscale is connected on both host and client devices, both belong to the same tailnet (or the node is explicitly shared), and MagicDNS + HTTPS Certificates are enabled at `https://login.tailscale.com/admin/dns`.
- **How secure is this?** We try to keep it secure, but it's only as secure as your Tailscale setup and is written using AI tools, so use it with caution. Securing your tailnet, ACLs, and bearer tokens is your responsibility.
