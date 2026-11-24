#!/usr/bin/env python3
"""Generate deterministic PNG and ICO icon assets for cloudedits installers.

Uses only the Python standard library (math, struct, zlib) so it works on any
host without third-party image libraries.
"""

from __future__ import annotations

import math
import os
import struct
import zlib


def clamp(v: float, lo: float = 0.0, hi: float = 1.0) -> float:
    return lo if v < lo else (hi if v > hi else v)


def smoothstep(edge0: float, edge1: float, x: float) -> float:
    t = clamp((x - edge0) / (edge1 - edge0))
    return t * t * (3.0 - 2.0 * t)


def sdf_round_rect(x: float, y: float, cx: float, cy: float, hx: float, hy: float, r: float) -> float:
    dx = abs(x - cx) - (hx - r)
    dy = abs(y - cy) - (hy - r)
    outside = math.hypot(max(dx, 0.0), max(dy, 0.0))
    inside = min(max(dx, dy), 0.0)
    return outside + inside - r


def sdf_circle(x: float, y: float, cx: float, cy: float, r: float) -> float:
    return math.hypot(x - cx, y - cy) - r


def sdf_cloud(x: float, y: float) -> float:
    # Normalized coordinates in [0, 1] x [0, 1] matching cloudedits.svg
    d1 = sdf_circle(x, y, 0.50, 0.43, 0.21)
    d2 = sdf_circle(x, y, 0.31, 0.53, 0.145)
    d3 = sdf_circle(x, y, 0.70, 0.54, 0.14)
    d4 = sdf_round_rect(x, y, 0.50, 0.59, 0.22, 0.095, 0.085)
    return min(d1, d2, d3, d4)


def blend(dst: tuple[float, float, float, float], src: tuple[float, float, float, float]) -> tuple[float, float, float, float]:
    sa = src[3]
    da = dst[3]
    out_a = sa + da * (1.0 - sa)
    if out_a <= 1e-6:
        return (0.0, 0.0, 0.0, 0.0)
    out_r = (src[0] * sa + dst[0] * da * (1.0 - sa)) / out_a
    out_g = (src[1] * sa + dst[1] * da * (1.0 - sa)) / out_a
    out_b = (src[2] * sa + dst[2] * da * (1.0 - sa)) / out_a
    return (out_r, out_g, out_b, out_a)


def sample_pixel(x: float, y: float, px: float) -> tuple[int, int, int, int]:
    aa = px * 1.25
    col = (0.0, 0.0, 0.0, 0.0)

    # Rounded square dark glass tile
    d_bg = sdf_round_rect(x, y, 0.5, 0.5, 0.453, 0.453, 0.21)
    a_bg = 1.0 - smoothstep(-aa, aa, d_bg)
    if a_bg > 0.0:
        t = clamp((x + y) * 0.5)
        r = 0.090 * (1.0 - t) + 0.043 * t
        g = 0.106 * (1.0 - t) + 0.051 * t
        b = 0.141 * (1.0 - t) + 0.071 * t
        col = blend(col, (r, g, b, a_bg))

        # Subtle border ring
        d_border = abs(d_bg + 0.01) - 0.01
        a_border = (1.0 - smoothstep(-aa, aa, d_border)) * a_bg
        if a_border > 0.0:
            col = blend(col, (0.165, 0.188, 0.235, a_border))

    # Warm amber cloud body
    d_cloud = sdf_cloud(x, y)
    a_cloud = 1.0 - smoothstep(-aa, aa, d_cloud)
    if a_cloud > 0.0:
        t = clamp((x - 0.2 + y - 0.2) * 0.8)
        r = 0.949 * (1.0 - t) + 0.851 * t
        g = 0.757 * (1.0 - t) + 0.561 * t
        b = 0.357 * (1.0 - t) + 0.141 * t
        col = blend(col, (r, g, b, a_cloud))

    # Lens cutout
    d_lens = sdf_circle(x, y, 0.50, 0.49, 0.108)
    a_lens = 1.0 - smoothstep(-aa, aa, d_lens)
    if a_lens > 0.0:
        col = blend(col, (0.059, 0.067, 0.082, a_lens))

    # Inner amber aperture ring
    d_ring = abs(sdf_circle(x, y, 0.50, 0.49, 0.064)) - 0.011
    a_ring = 1.0 - smoothstep(-aa, aa, d_ring)
    if a_ring > 0.0:
        col = blend(col, (0.949, 0.757, 0.357, a_ring))

    # Bright specular center
    d_dot = sdf_circle(x, y, 0.50, 0.49, 0.024)
    a_dot = 1.0 - smoothstep(-aa, aa, d_dot)
    if a_dot > 0.0:
        col = blend(col, (0.925, 0.937, 0.957, a_dot))

    return (
        int(round(clamp(col[0]) * 255.0)),
        int(round(clamp(col[1]) * 255.0)),
        int(round(clamp(col[2]) * 255.0)),
        int(round(clamp(col[3]) * 255.0)),
    )


def encode_png(size: int) -> bytes:
    px = 1.0 / size
    raw = bytearray()
    for iy in range(size):
        raw.append(0)  # filter type 0
        y = (iy + 0.5) * px
        for ix in range(size):
            x = (ix + 0.5) * px
            r, g, b, a = sample_pixel(x, y, px)
            raw.extend((r, g, b, a))

    def chunk(tag: bytes, data: bytes) -> bytes:
        crc = zlib.crc32(tag + data) & 0xFFFFFFFF
        return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", crc)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    idat = zlib.compress(bytes(raw), level=9)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")


def encode_ico(images: list[tuple[int, bytes]]) -> bytes:
    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    entries = bytearray()
    payload = bytearray()
    for size, png_data in images:
        dim = 0 if size >= 256 else size
        entries.extend(struct.pack("<BBBBHHII", dim, dim, 0, 0, 1, 32, len(png_data), offset))
        payload.extend(png_data)
        offset += len(png_data)
    return header + bytes(entries) + bytes(payload)


def main() -> None:
    out_dir = os.path.dirname(os.path.abspath(__file__))

    png_1024 = encode_png(1024)
    with open(os.path.join(out_dir, "cloudedits-1024.png"), "wb") as f:
        f.write(png_1024)

    png_256 = encode_png(256)
    with open(os.path.join(out_dir, "cloudedits-256.png"), "wb") as f:
        f.write(png_256)

    ico_frames = [(s, png_256 if s == 256 else encode_png(s)) for s in (16, 32, 48, 256)]
    with open(os.path.join(out_dir, "cloudedits.ico"), "wb") as f:
        f.write(encode_ico(ico_frames))

    print("Generated cloudedits-1024.png, cloudedits-256.png, cloudedits.ico")


if __name__ == "__main__":
    main()
