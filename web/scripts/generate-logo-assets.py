#!/usr/bin/env python3
"""Rasterise the Sync brand mark into the PNG/ICO assets shipped by the SPA.

The mark is *defined in code* (same 24x24 grid as the SVG sources, 2px strokes,
rounded caps) so the raster assets always match `logo.svg`
without any external design tool. Rerun after editing the geometry:

    python3 web/scripts/generate-logo-assets.py

Outputs (all inside web/public):
    logo.png              1024x1024  transparent gradient mark
    apple-touch-icon.png   180x180   mark on an opaque rounded square
    favicon.ico           16/32/48   multi-resolution icon

Only Pillow is required (it is a design-time tool, never a runtime dependency).
"""

from __future__ import annotations

import pathlib

from PIL import Image, ImageDraw

# ---------------------------------------------------------------------------
# Brand geometry — keep in sync with web/public/logo.svg
# ---------------------------------------------------------------------------
GRID = 24.0                     # SVG user units of the icon grid
STROKE = 2.0                    # 2px line, rounded caps (see docs/branding.md)
PADDING = 0.15                  # optical margin around the mark, ratio of canvas

# Right arrow (top) then left arrow (bottom): the bidirectional exchange.
SEGMENTS = (
    ((4.0, 8.5), (19.0, 8.5)),      # top shaft →
    ((15.5, 5.0), (19.0, 8.5)),     # top head
    ((19.0, 8.5), (15.5, 12.0)),
    ((20.0, 15.5), (5.0, 15.5)),    # bottom shaft ←
    ((8.5, 12.0), (5.0, 15.5)),     # bottom head
    ((5.0, 15.5), (8.5, 19.0)),
)

GRADIENT_FROM = (79, 70, 229)    # #4F46E5 indigo-600
GRADIENT_TO = (6, 182, 212)      # #06B6D4 cyan-500

SUPERSAMPLE = 4                  # anti-aliasing factor
OUTPUT_DIR = pathlib.Path(__file__).resolve().parents[1] / "public"


def mark_mask(size: int) -> Image.Image:
    """Return an L-mode alpha mask of the mark at the requested size."""
    canvas = size * SUPERSAMPLE
    mask = Image.new("L", (canvas, canvas), 0)
    draw = ImageDraw.Draw(mask)

    scale = canvas * (1.0 - 2.0 * PADDING) / GRID
    offset = canvas * PADDING
    line_width = max(1, round(STROKE * scale))

    def point(x: float, y: float) -> tuple[float, float]:
        return offset + x * scale, offset + y * scale

    for start, end in SEGMENTS:
        draw.line([point(*start), point(*end)], fill=255, width=line_width)

    # Rounded caps: PIL draws flat ends, so close every vertex with a disc.
    radius = line_width / 2.0
    vertices = {vertex for segment in SEGMENTS for vertex in segment}
    for x, y in vertices:
        cx, cy = point(x, y)
        draw.ellipse([cx - radius, cy - radius, cx + radius, cy + radius], fill=255)

    return mask.resize((size, size), Image.LANCZOS)


def gradient(size: int) -> Image.Image:
    """Diagonal (135°) brand gradient, generated small and upscaled for speed."""
    small = 256
    ramp = Image.new("RGB", (small, small))
    pixels = ramp.load()
    for y in range(small):
        for x in range(small):
            t = (x + y) / (2.0 * (small - 1))
            pixels[x, y] = tuple(
                round(a + (b - a) * t) for a, b in zip(GRADIENT_FROM, GRADIENT_TO)
            )
    return ramp.resize((size, size), Image.BICUBIC).convert("RGBA")


def gradient_mark(size: int) -> Image.Image:
    """Transparent canvas with the gradient-coloured mark."""
    mark = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    mark.paste(gradient(size), (0, 0), mark_mask(size))
    return mark


def rounded_square(size: int, radius_ratio: float = 0.22) -> Image.Image:
    """Opaque rounded-square plate used for the touch icon."""
    plate = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    ImageDraw.Draw(plate).rounded_rectangle(
        [0, 0, size - 1, size - 1],
        radius=round(size * radius_ratio),
        fill=(255, 255, 255, 255),
    )
    return plate


def main() -> None:
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    logo = gradient_mark(1024)
    logo.save(OUTPUT_DIR / "logo.png", optimize=True)

    touch = rounded_square(180)
    touch.alpha_composite(gradient_mark(180))
    touch.save(OUTPUT_DIR / "apple-touch-icon.png", optimize=True)

    favicon = gradient_mark(256)
    favicon.save(
        OUTPUT_DIR / "favicon.ico",
        sizes=[(16, 16), (32, 32), (48, 48)],
    )

    for name in ("logo.png", "apple-touch-icon.png", "favicon.ico"):
        print(f"wrote {OUTPUT_DIR / name}")


if __name__ == "__main__":
    main()
