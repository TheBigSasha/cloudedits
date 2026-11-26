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
    │   ├── IMG_0001.ARW.edit_thumbnail.jpg  # Disposable edited preview JPEG (UltraHDR when HDR)
    │   └── IMG_0001.ARW.depth.png           # Auxiliary channel PNG sidecar (e.g. depth buffer)
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
- **Comparison order:** Parse `unixMillis` as `uint64` and `counter` as `uint32`. Compare `unixMillis`, then `counter`, then `deviceId` in UTF-16 code-unit order (the order of the ECMAScript `<` operator). Never compare raw HLC strings lexicographically, because variable-width hex (`ff` vs `100`) would sort incorrectly.

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

The canonical bytes of a sidecar are exactly what `JSON.stringify(value, null, 2) + "\n"` produces in an ECMAScript engine for a recursively key-sorted object. A browser editor emits that form natively; every other implementation must reproduce the same four rules, or two tools rewriting one file will never agree on its bytes or on a merge tie-break.

1. **Key order.** Within every object, keys that are array indices (the canonical decimal form of an integer in `0 … 2^32-2`, such as `"0"`, `"9"`, `"10"`) come first in ascending numeric order; all other keys follow in UTF-16 code-unit order. UTF-16 order differs from UTF-8 byte order when a supplementary-plane character meets one in `U+E000–U+FFFF`.
2. **`hdrt` first.** At the top level `hdrt` precedes every non-index key.
3. **Strings.** Only `"`, `\`, and the C0 controls are escaped (`\b \t \n \f \r`, otherwise `\u00xx` in lowercase hex). `<`, `>`, `&`, `U+007F`, `U+2028`, and `U+2029` are written raw.
4. **Numbers.** Every number is written as the shortest decimal that round-trips an IEEE-754 double: `1.0` → `1`, `-0` → `0`, `1e21` → `1e+21`, `1.5e-07` → `1.5e-7`. Integers beyond `2^53` therefore lose precision, and a literal outside the double range becomes `null`.
5. `fields` is always emitted (`{}` when empty).
6. `variants` is stably sorted ascending by `id` in UTF-16 code-unit order and omitted when empty.
7. 2-space indentation with a single trailing newline (`\n`).
8. **Additive-only & unknown-field preservation:** Readers accept any `hdrt >= 1` and preserve all unknown top-level keys, unknown `fields` keys, and unknown properties on `document`, `asset`, `fields[*]`, `recipe`, and `variants[*]` through parse -> mutate -> serialize. A field cell without a `v` member stays without one, and a variant `name`, `recipe`, or `tombstone` of an unexpected JSON type is re-emitted verbatim.

`testdata/reference_vectors/vectors.jsonl` freezes inputs together with the bytes the browser editor produces for them (parse -> serialize, op application, merge, conflict rescue, HLC parse/compare/format). `go test ./internal/sidecar` fails on any byte of drift.

### Known differences from the browser editor

- A lone UTF-16 surrogate escape (`"\ud800"`) cannot be represented in a Go string and is read as `U+FFFD`.
- HLC strings are validated on read. A sidecar whose cell carries a malformed HLC is quarantined here, while the browser editor loads it and fails only when it next compares that cell.
- A `variant` op keeps every member of its value (for example `snapshotKind`), and keys named like `Object.prototype` members (`__proto__`, `constructor`, `toString`) are ordinary data. Browser editor builds that predate the reference vectors kept only `id`, `name`, `recipe`, and `tombstone` of a variant op and dropped or mangled those keys.

## Deterministic Merge Lattice

Merging two sidecars for the same asset is a join-semilattice (associative, commutative, idempotent):

- `hdrt`: `max(a.hdrt, b.hdrt)`.
- `document`: `role` only (`truth` if either copy is truth). A portable copy never merges into a truth copy; the truth copy is returned unchanged.
- `fields.*`: per-cell LWW by numeric HLC; exact HLC ties break by larger canonical compact JSON of the cell, compared in UTF-16 code-unit order. `keywords` is LWW on the whole array.
- `recipe`: LWW by numeric HLC (same tie-break). **Conflict rescue:** when two truth copies carry recipes with distinct canonical JSON and the losing value is a JSON object, the losing recipe is preserved as an immutable manual variant with:
  - `id`: `"conflict-<loserHlc>-<32-hex-digit content hash>"`. The hash input is the compact JSON `{"hlc":<loserHlc>,"recipe":<loserRecipe>}` with keys in plain UTF-16 code-unit order (no array-index hoisting); the hash is four independently seeded 32-bit lanes (`0x811c9dc5`, `0x9e3779b9`, `0x85ebca6b`, `0xc2b2ae35`), each updated per UTF-16 code unit `c` of that string as `h = (h XOR (c + lane*257)) * 0x01000193 mod 2^32; h = h XOR (h >> 16)`, printed as 8 lowercase hex digits per lane.
  - `name`: `"Merged edit"`, or `"Merged edit · from .hdrtoys"` / `"Merged edit · from beside the photo"` when the two copies came from the two on-disk layouts.
  - `snapshotKind`: `"manual"`
- `variants`: append-only union keyed by `id`. Payload winner is selected by `(hlc, canonical payload JSON without tombstone)`; `tombstone` is logical OR (`a.tombstone || b.tombstone`) so deletions dominate forever regardless of HLC order.
- `asset`, unknown top-level keys: per-key join where `null` is the bottom element and non-null ties resolve by larger canonical JSON.
- `provenance`: joined as one value under the same rule.

## Two On-Disk Layouts, One Truth

A browser editor may keep an asset's sidecar in either of two places, and older versions always used the second:

1. `<sidecarRoot>/<rel>.edits` — the canonical sidecar, the only file `cloudedits` writes.
2. Beside the photo: `<photoFolder>/<rel>.edits`, else `<photoFolder>/<rel>.hdrt.json` (legacy JSON; a missing `document` is read as `role: truth`).

A folder can therefore hold a copy in each layout. Every read joins the two with the merge lattice above, including conflict rescue with the origin-labelled name, which is exactly the document the browser editor computes for the same pair of files. Reading only one of them would show stale ratings, flags, or recipes whenever the other copy is newer.

- The copy beside the photo is strictly read-only: it is never written, moved, or removed.
- A portable document beside the photo (`document.role: "portable"`) is a shared edit file, not folder truth, and is ignored.
- Writes **always** commit the joined state atomically to `<sidecarRoot>/<rel>.edits` via temp file in `<sidecarRoot>/.tmp/<uuid>` -> `fsync` -> `close` -> `rename` -> `fsync` of the target directory. Only then is a write acknowledged as durable. Setting `CLOUDEDITS_FSYNC=0` skips both flushes: writes stay atomic against a crash of the server process, but an OS crash or power loss can lose or truncate an acknowledged edit. Staging files orphaned by a killed process are swept from `.tmp/` once they are 15 minutes old.

If `<sidecarRoot>/<rel>.edits` exists but fails JSON or schema parsing, it is moved into `<sidecarRoot>/.quarantine/<timestamp>-<rel>.edits` rather than being silently masked by the other copy.

## Auxiliary Channels (`<rel>.<channel>.png`)

Disposable auxiliary channel PNGs (starting with `depth`) live next to `.edits` inside `.hdrtoys/`:

- **Canonical path:** `<sidecarRoot>/<rel-posix-path>.<channel>.png` (for example `.hdrtoys/DCIM/IMG_0001.ARW.depth.png`).
- **Colocated read fallback:** If the canonical file is absent, readers fall back to a read-only colocated `<photoFolder>/<rel-posix-path>.<channel>.png`. Colocated auxiliary files are excluded from library scanning and are never indexed as standalone photos.
- **Writes and deletes:** Writes always commit atomically into `<sidecarRoot>` via `<sidecarRoot>/.tmp/<uuid>` -> `fsync` -> `close` -> `rename` -> directory `fsync`. Deletes remove only `<sidecarRoot>/<rel-posix-path>.<channel>.png`; source photo folders remain strictly read-only.
