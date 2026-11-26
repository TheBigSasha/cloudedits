# hdr.toys Remote Library HTTP API v1 Contract

`cloudedits` implements the **hdr.toys Remote Library HTTP API v1** with the `chrome-local-parity/1` profile.

## Headers Law

Every HTTP response (JSON, binary blobs, redirects, and errors) includes:

- `Cross-Origin-Resource-Policy: cross-origin`
- `Access-Control-Allow-Origin` (reflecting allowed origin or `*`)
- `Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS`
- `Access-Control-Allow-Headers: Authorization, Content-Type, Range, If-Range, If-None-Match, Idempotency-Key, X-HDRT-Profile, X-HDRToys-Client, X-HDRToys-Image-Id, X-HDRToys-Render-Id, X-HDRToys-Variant-Id, X-HDRToys-Channel, X-HDRToys-Filename, X-HDRToys-Schema-Version`
- `Access-Control-Expose-Headers: Content-Disposition, Content-Range, ETag, X-HDRT-Source-Revision, X-HDRT-Tier-Source, Server-Timing`
- `Server-Timing: app;dur=<ms>`

## Original Blob Streaming Law (`?tier=original`)

Requests to `GET /assets/{id}/blob?tier=original` (and alias `GET /assets/{id}/file?tier=original`) MUST satisfy:

1. `Cache-Control: private, no-store` — prevents mobile/WebKit caches from retaining multi-hundred-megabyte RAW buffers.
2. `Content-Encoding: identity` — never gzip/br-compress original media bytes so byte ranges remain deterministic.
3. `Accept-Ranges: bytes` with full `206 Partial Content` and `If-Range` support.
4. Strong quoted `ETag: "<sourceRevision>"` matching `X-HDRT-Source-Revision: <sourceRevision>`.
5. Immediate read/stream cancellation when the client aborts (`r.Context().Done()`).

## Thumbnail Tier Priority (`?tier=t0|t1|t2|thumb|preview`)

1. If `.hdrtoys/<rel>.edit_thumbnail.jpg` exists, serve it verbatim with `X-HDRT-Tier-Source: edit-thumbnail` (preserving UltraHDR gain maps when present).
2. Otherwise serve cached thumbnail from app-data disk cache (`X-HDRT-Tier-Source: embedded` or `generated`).
3. Otherwise extract embedded JPEG preview from RAW/EXIF/TIFF/CR3/RAF (never full RAW decode) or generate a bounded raster downscale (<= 640 px long edge, JPEG q82).
4. Emit strong `ETag`, support `If-None-Match` -> `304 Not Modified`, and set `Cache-Control: private, max-age=3600, stale-while-revalidate=86400`.

## Endpoints

| Method | Path | Auth | Permission | Purpose |
|---|---|---|---|---|
| `GET` | `/` | Public | — | `302` redirect to `https://hdr.toys/?gallery_api_url=<self>&gallery_name=<name>` (or JSON if `Accept: application/json`) |
| `GET` | `/open` | Public | — | `302` redirect to `https://hdr.toys/?gallery_api_url=<self>&image_id=<a>` |
| `GET` | `/h/{id}` | Public | — | `302` redirect to `https://hdr.toys/?gallery_api_url=<self>&handoff=<id>` |
| `GET` | `/capabilities` | Public | — | Advertise `protocol` (`hdrtoys-lp` v2), `profiles`, `caps`, `permissions`, `limits`, and `library` |
| `POST` | `/auth/session` | Public | — | Exchange Tailnet WhoIs identity or bearer token for a short-lived session token and blob signing key scope |
| `GET` | `/assets/count` | Authenticated | `library:read` | Count matching assets (`{ total, count, exact }`) |
| `GET` | `/assets/range` | Authenticated | `library:read` | Offset/limit slice (`{ total, exact, offset, limit, assets[] }`, `limit <= 500`, `fields=grid|full`) |
| `GET` | `/assets` | Authenticated | `library:read` | Cursor-paginated list (`{ assets[], nextCursor, total }`, `limit <= 200`) |
| `GET` | `/assets/{id}` | Authenticated | `asset:read` | Full asset metadata (including `thumbnailUrl`, `rawUrl`, `recipeUrl`, `editedByHdrToys`, full `exif`) |
| `GET` | `/assets/{id}/blob` | Authenticated | `asset:read` | Asset bytes (`?tier=original|t0|t1|t2|thumb|preview`, supports `&bt=<signed>`) |
| `GET` | `/assets/{id}/file` | Authenticated | `asset:read` | Alias for `/assets/{id}/blob` |
| `GET` | `/assets/{id}/recipe` | Authenticated | `asset:read` | Current working recipe envelope `{ assetId, recipe, hlc }` (`404` `not-found` when unedited) |
| `POST` | `/assets/{id}/rating` | Authenticated | `edits:write` | Write asset rating (`0`–`5`) durably as an HLC op |
| `GET` | `/assets/{id}/variants` | Authenticated | `asset:read` | Active immutable variants (`{ id, name, hlc, recipeUrl, recipe }`; pass `?includeTombstones=true` for deleted entries) |
| `GET` | `/assets/{id}/variants/{variantId}/recipe` | Authenticated | `asset:read` | Fetch one variant's recipe JSON |
| `POST` | `/ops` | Authenticated | `edits:write` | Canonical batch HLC mutation endpoint (accepts raw `[op, ...]` or `{ "ops": [...] }`; returns `{ durable, applied, duplicates, stale, receipts }`) |
| `GET` | `/changes` | Authenticated | `library:read` | Monotonic change feed `?since=<cursor>&limit=<n<=500>` -> `{ ops[], assets[], nextCursor, snapshotRequired? }` |
| `PUT` | `/assets/{id}/edit-thumbnail` | Authenticated | `media:write` | Persist edited JPEG preview (<= 4 MiB, `FF D8` verified) |
| `POST` | `/assets/{id}/renders` | Authenticated | `variants:write` | Idempotent rendered export upload (`Idempotency-Key` header or `renderId` query; owning variant from `variantId` query or `X-HDRToys-Variant-Id` header) |
| `GET` | `/assets/{id}/renders` | Authenticated | `asset:read` | List stored rendered exports for an asset |
| `DELETE` | `/assets/{id}/variants/{variantId}` | Authenticated | `variants:write` | Append a tombstone op for a variant |
| `POST` | `/handoff` | Authenticated | `handoff:create` | Create ephemeral 5-minute UI state handoff (`8`-char base62 ID) |
| `GET` | `/handoff/{id}` | Authenticated | `asset:read` | Fetch ephemeral UI state handoff payload |

## Filtering & Sorting Query Parameters

Supported on `/assets/count`, `/assets/range`, and `/assets`:

- `q`: case-insensitive substring match against filename, relative path, or keywords
- `rating`: exact integer rating (`0`–`5`)
- `rating_gte`: minimum integer rating (`0`–`5`)
- `flag`: `pick`, `reject`, `any` (any flagged), or `none`
- `label`: color label string (e.g. `red`, `yellow`, `green`, `blue`, `purple`), `any` (any labeled), or `none`
- `keyword`: repeatable (`&keyword=tokyo&keyword=night`), matched with logical AND
- `kind`: repeatable or comma-separated, matched with logical OR: `raw`, `image`, `photo` (every non-RAW asset), or a file extension (`jpeg` covers `.jpg`/`.jpeg`, `tiff` covers `.tif`/`.tiff`, `heif` covers `.heic`/`.heif`/`.hif`)
- `folder`: folder ID or folder label filter
- `sort`: `captured`, `imported`, `edited`, `name`, `rating` (default `captured`); the single-token forms `<field>_asc` / `<field>_desc` (for example `name_asc`, `rating_desc`) are accepted as well
- `dir`: `asc` or `desc` (default `desc`; overrides a `sort` token suffix)
- `fields`: `grid` (compact) or `full`

## Change Feed (`GET /changes`)

- `since` is the opaque `nextCursor` of the previous page; omit it to bootstrap from the start of the journal.
- Each page carries the applied ops after the cursor (oldest first), the current summaries of the assets they touched, and the `nextCursor` to poll with next. An empty `ops` array with an unchanged `nextCursor` means the client is caught up.
- `snapshotRequired: true` means the cursor is not a position in this server's journal — the rebuildable index was re-created after the cursor was issued. The client must re-run its initial sync (`GET /assets` or `/assets/range`) and continue polling from the returned `nextCursor`.

## Batch Operations (`POST /ops`)

Request body:

```json
{
  "ops": [
    {
      "hlc": "019a2f0012ab-0001-devA",
      "device": "devA",
      "assetKey": "<assetId>",
      "field": "rating",
      "value": 5
    }
  ]
}
```

Response body:

```json
{
  "durable": true,
  "receipts": [
    {
      "device": "devA",
      "hlc": "019a2f0012ab-0001-devA",
      "status": "applied"
    }
  ]
}
```

Limits: at most `limits.maxOpsBatch` ops and `limits.maxOpsBodyBytes` request-body bytes per call (both advertised by `GET /capabilities`). A request over either limit is rejected whole with `413` and error code `payload-too-large`; split the batch and retry.

Receipt `status` values:
- `applied`: accepted and written atomically to `.hdrtoys/<rel>.edits`.
- `duplicate`: `(device, hlc)` was already processed (`originalStatus` included).
- `stale`: rejected because an equal or newer HLC already won on disk.
