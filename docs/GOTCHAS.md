# Implementer Gotchas

Hard-won lessons for anyone implementing or operating a `hdr.toys` Remote Library server.

## 1. Mobile WebKit / Safari Original Caching (`Cache-Control: private, no-store`)

Mobile Safari's `WebKit.Networking` / `NSURLCache` can attempt to cache large `200 OK` binary responses in memory or temporary disk storage, triggering jetsam memory pressure when browsing 50–150 MB RAW or HDR images. Always emit `Cache-Control: private, no-store` on `?tier=original` and stop writing immediately when `r.Context().Done()` fires.

## 2. Range Requests, `If-Range`, and `Content-Encoding: identity`

Resumable browser download caches validate byte ranges strictly:
- Always serve `?tier=original` with `Content-Encoding: identity` (never wrap original binary responses in gzip/brotli middleware).
- Always emit a strong quoted `ETag: "<sourceRevision>"` and matching `X-HDRT-Source-Revision`.
- Support `Range: bytes=start-end` (`206 Partial Content` + `Content-Range`) and `If-Range` so a file change on disk invalidates stale partial ranges cleanly.

## 3. Cross-Origin Resource Policy (CORP)

Cross-origin image/fetch requests from `https://hdr.toys` when cross-origin isolation (`COOP`/`COEP`) is active fail silently unless **every** response — including `304`, `206`, and error JSON — emits `Cross-Origin-Resource-Policy: cross-origin` alongside CORS headers.

## 4. AppleDouble (`._*`) and Hidden Metadata Files

macOS writes `._IMG_0001.JPG` resource-fork sidecar files onto SMB, exFAT, and external drives. They share media extensions (`.jpg`, `.arw`, `.dng`) and will break scanners and thumbnailers if not skipped. Always exclude basenames starting with `._`, `.DS_Store`, and the `.hdrtoys/` directory itself.

## 5. Embedded RAW Thumbnails vs Full RAW Decode

Never decode a 60 MP camera RAW on the server just to draw a 640 px grid thumbnail. Every modern RAW container (TIFF/EXIF-based DNG/CR2/NEF/ARW/ORF/RW2/PEF/SRW, ISOBMFF CR3, Fujifilm RAF) embeds a ready-to-serve JPEG preview. Scan offsets directly in pure Go for sub-120 ms cold thumbnails with tiny memory overhead.

## 6. Atomic `.edits` Durability

Never overwrite `.hdrtoys/<rel>.edits` in place (`os.WriteFile`). A crash, power loss, or concurrent local editor read can observe truncated JSON. Always stage into `<sidecarRoot>/.tmp/<uuid>`, `Sync()`, `Close()`, `os.Rename()` on the same filesystem, and then `Sync()` the target directory: without the first flush a power loss can leave a zero-length file under the final name, and without the second the rename itself can be lost after the client was told the write is durable. If a file on disk is corrupt, quarantine it to `.hdrtoys/.quarantine/` instead of silently discarding user edits.

## 7. Numeric HLC Ordering

Never compare HLC strings with plain `<` / `>`. A counter or timestamp with different hex width (`ff` vs `100`) sorts backwards lexically. Always parse `(millis uint64, counter uint32, device string)` and compare numerically.

## 8. Rebuildable SQLite WAL Hygiene

Keep `index.db` outside the photo folder (never on SMB/NFS network shares where SQLite file locking is unreliable). Enable `PRAGMA journal_mode=WAL`, `PRAGMA synchronous=NORMAL`, and `PRAGMA busy_timeout=5000`. Treat the database as strictly disposable cache/index derived from source files + `.hdrtoys/*.edits`.

## 9. `tsnet` MagicDNS & HTTPS Certificates

`tsnet.Server.ListenTLS("tcp", ":443")` requires both **MagicDNS** and **HTTPS Certificates** to be enabled in the Tailscale admin console (`https://login.tailscale.com/admin/dns`). Check `len(srv.CertDomains()) > 0` after `srv.Up(ctx)` and return an actionable error (exit code `3` in CLI) rather than blocking indefinitely.

## 10. Read-Only NAS Bind Mounts

By default, `cloudedits` writes sidecars to `<folder>/.hdrtoys/` so local folder access and remote access share edits seamlessly. If a NAS administrator insists on mounting `/library/photos:ro`, pass `--sidecar-dir /data/sidecars` so `cloudedits` stays functional without failing on write; `cloudedits doctor` explicitly notes that external sidecars are not colocated inside the photo folder.

## 11. A Second Writer on the Same Folder

A browser editor with direct file access can have the same photo folder open while the server runs, and both rewrite `.hdrtoys/<rel>.edits` with read-merge-write. The per-asset mutex only serializes this server's own writers. Staging a rewrite (write + `fsync`) can take tens of milliseconds, long enough for the other process to save an edit after the server read the file; renaming over it would discard an edit that process already reported as saved. The server therefore compares the sidecar with the bytes it read immediately before the rename and restarts its read-merge-write when they differ. After six attempts it writes nothing and answers `503` with `Retry-After`, so the client resends.

What remains is the interval between that comparison and the rename (two system calls), and the mirror image on the other side: a browser cannot take a file lock, so a browser editor can still replace a write the server made after the browser's own read. Both sides merge on every read, so such a lost write is repaired the next time its author saves, but until then it is missing from the file.
