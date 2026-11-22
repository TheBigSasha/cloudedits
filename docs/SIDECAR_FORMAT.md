# `.hdrtoys/<rel-posix-path>.edits` Sidecar Format Specification

`cloudedits` stores all non-destructive photo metadata and edits in canonical JSON sidecars inside `<folder>/.hdrtoys/` so original media files remain strictly read-only and travel cleanly across devices.

## Directory Layout

```text
<photo-folder>/
├── DCIM/
│   ├── IMG_0001.ARW                         # Strictly read-only
│   └── IMG_0001.JPG                         # Strictly read-only
└── .hdrtoys/                                # Only writable directory inside photo folder
    ├── DCIM/
    │   ├── IMG_0001.ARW.edits               # Canonical truth sidecar (hdrt: 1)
    │   └── IMG_0001.ARW.edit_thumbnail.jpg  # Disposable edited preview JPEG (UltraHDR when HDR)
    ├── renders/                             # Optional uploaded rendered exports
    ├── .tmp/                                # Same-filesystem atomic rename staging
    └── .quarantine/                         # Corrupt .edits files moved here instead of silently dropped
```

## Hybrid Logical Clock (HLC) Format

```text
<unixMillis-hex>-<counter-hex>-<deviceId>
```

- `unixMillis-hex`: lowercase hex Unix milliseconds, zero-padded to at least 12 hex digits (`%012x`).
- `counter-hex`: lowercase hex logical counter, zero-padded to at least 4 hex digits (`%04x`).
- `deviceId`: non-empty string identifying the originating device. `deviceId` may contain `-`; parsers MUST split only on the **first two** `-` characters.
- **Comparison order:** Parse `unixMillis` as `uint64` and `counter` as `uint32`. Compare `unixMillis`, then `counter`, then `deviceId` lexicographically. Never compare raw HLC strings lexicographically, because variable-width hex (`ff` vs `100`) would sort incorrectly.

## JSON Envelope (`hdrt: 1`)

```json
{
  "hdrt": 1,
  "asset": {
    "bytes": 51042816,
    "file": "IMG_0001.ARW",
    "hash": "xxh3:0123456789abcdef"
  },
  "document": {
    "role": "truth"
  },
  "fields": {
    "colorLabel": {
      "hlc": "019a2f0012ab-0001-devA",
      "v": "red"
    },
    "flag": {
      "hlc": "019a2f0012ab-0002-devA",
      "v": "pick"
    },
    "keywords": {
      "hlc": "019a2f0012ab-0003-devA",
      "v": [
        "tokyo",
        "night"
      ]
    },
    "rating": {
      "hlc": "019a2f0012ab-0004-devA",
      "v": 5
    }
  },
  "recipe": {
    "hlc": "019a2f0019ff-0001-devA",
    "v": {
      "version": 1,
      "exposure": 0.4
    }
  },
  "variants": [
    {
      "hlc": "019a2f0019ff-0002-devA",
      "id": "v1",
      "name": "Warm Night",
      "recipe": {
        "version": 1,
        "exposure": 0.4
      },
      "snapshotKind": "manual"
    }
  ]
}
```

## Canonical Serialization Rules

1. Top-level key `hdrt` is always emitted **first**.
2. All other top-level keys and all nested JSON object keys are recursively sorted lexicographically (UTF-8 / code-unit order).
3. `fields` is always emitted (`{}` when empty).
4. `variants` is sorted ascending by `id` and omitted when empty.
5. 2-space indentation with a single trailing newline (`\n`).
6. **Additive-only & unknown-field preservation:** Readers accept any `hdrt >= 1` and preserve all unknown top-level keys, unknown `fields` keys, and unknown properties on `document`, `asset`, `fields[*]`, `recipe`, and `variants[*]` untouched through parse -> mutate -> serialize.

## Deterministic Merge Lattice

Merging two sidecars for the same asset is a join-semilattice (associative, commutative, idempotent):

- `hdrt`: `max(a.hdrt, b.hdrt)`.
- `fields.*`: per-cell LWW by numeric HLC; exact HLC ties break by larger canonical compact JSON bytes. `keywords` is LWW on the whole array.
- `recipe`: LWW by numeric HLC (same tie-break). **Conflict rescue:** when merging two concurrent truth sidecars that contain valid recipes with distinct canonical JSON bytes, the losing recipe is preserved automatically as an immutable manual variant with:
  - `id`: `"conflict-<loserHlc>-<8-char-sha256-prefix>"`
  - `name`: `"Merged edit"`
  - `snapshotKind`: `"manual"`
- `variants`: append-only union keyed by `id`. Payload winner is selected by `(hlc, canonical payload JSON without tombstone)`; `tombstone` is logical OR (`a.tombstone || b.tombstone`) so deletions dominate forever regardless of HLC order.
- `asset`, `provenance`, unknown top-level keys: per-key join where `null` is the bottom element and non-null ties resolve by larger canonical JSON bytes.

## Legacy Read Precedence & Quarantine

When reading an asset's sidecar, `cloudedits` checks in order:

1. `<sidecarRoot>/<rel>.edits` (canonical)
2. `<photoFolder>/<rel>.edits` (colocated legacy)
3. `<photoFolder>/<rel>.hdrt.json` (legacy JSON)

Writes **always** commit atomically to `<sidecarRoot>/<rel>.edits` via temp file in `<sidecarRoot>/.tmp/<uuid>` -> `fsync` -> `close` -> `rename`.

If `<sidecarRoot>/<rel>.edits` exists but fails JSON or schema parsing, it is moved into `<sidecarRoot>/.quarantine/<timestamp>-<rel>.edits` rather than being silently masked by a legacy file.
