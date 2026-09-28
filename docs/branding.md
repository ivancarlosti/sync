# Branding

The identity of Sync is defined in **code and SVG**, not in a design tool: the
mark is a symmetric, direction-agnostic glyph (it reads the same mirrored, which
is what a product shipped in RTL needs) with 2 px strokes on a 24×24 grid.

## 1. Assets

| File | Size | Used by |
|---|---|---|
| `web/public/logo.svg` | vector | the header (`AppHeader.vue`, `h-7 w-7`) and the login card (`LoginView.vue`, `h-12 w-12`), plus `mask-icon` |
| `web/public/logo.png` | 1024×1024 | `og:image`, `twitter:image` (social preview) |
| `web/public/favicon.svg` | vector | `<link rel="icon" type="image/svg+xml">` |
| `web/public/favicon.ico` | 16/32/48 | `<link rel="alternate icon">` for browsers without SVG icons |
| `web/public/apple-touch-icon.png` | 180×180 | iOS home screen (opaque rounded square) |
| `web/public/robots.txt` | — | keeps crawlers out of `/api/` and the private screens |
| `web/src/assets/logo.svg`, `logo-icon.svg` | vector | in-app usage where a Vite-managed asset is preferable (theme-aware contexts) |

`index.html` also declares the theme colours
(`#ffffff` light / `#0b1120` dark) and `color-scheme: light dark`, which is what
tints the browser UI before the SPA paints.

## 2. Regenerating the raster assets

The PNG/ICO files are produced from the same geometry as the SVG sources by a
small script (Pillow is required; it is a design-time dependency, never part of
the build):

```bash
python3 web/scripts/generate-logo-assets.py
```

It writes `logo.png` (transparent gradient mark), `apple-touch-icon.png` (mark on
an opaque rounded square) and `favicon.ico` (multi-resolution) into `web/public`.

## 3. Theme tokens

Colours are HSL channel triplets in CSS custom properties
(`web/src/style.css`), consumed by Tailwind through `hsl(var(--token))`
(`web/tailwind.config.js`), so a component says `bg-primary/90` and both themes
follow.

| Token | Light | Dark | Meaning |
|---|---|---|---|
| `--background` / `--foreground` | `0 0% 100%` / `222 47% 11%` | `222 47% 11%` / `210 40% 98%` | page |
| `--card` / `--popover` | `0 0% 100%` | `222 45% 14%` | surfaces |
| `--primary` | `221 83% 53%` | `217 91% 60%` | brand blue, buttons, active nav |
| `--secondary`, `--muted`, `--accent` | `210 40% 96%` | `217 33% 20%` | quiet surfaces |
| `--destructive` | `0 72% 51%` | `0 63% 50%` | failures, delete actions |
| `--success` | `142 71% 45%` | `142 69% 45%` | `connected`, `success` badges |
| `--warning` | `38 92% 50%` | `38 92% 55%` | `partial`, degraded |
| `--border`, `--input`, `--ring` | `214 32% 91%` / `221 83% 53%` | `217 33% 24%` / `217 91% 60%` | outlines and focus |
| `--radius` | `0.65rem` | same | `rounded-lg/md/sm` derive from it |

`src/stores/theme.ts` toggles the `dark` class (and `color-scheme`) on `<html>`;
the inline script in `index.html` applies the stored preference before the first
paint, so a dark-mode reload never flashes white.

## 4. Where the product name appears

| Place | Value |
|---|---|
| `internal/version.Name` | `sync` (reported by `/api/version`) |
| `<title>` and `og:site_name` | `Sync` |
| Catalogs `common.appName` | translated (`Sync` in every language today) |
| Docs, header, login card | `Sync` |

## 5. Rebranding checklist

1. Replace `web/public/logo.svg` + `favicon.svg`, then run
   `python3 web/scripts/generate-logo-assets.py` for the PNG/ICO trio.
2. Adjust `--primary` (and friends) in `web/src/style.css`; keep the light/dark
   pairs distinct enough for the WCAG contrast the components rely on.
3. Update `theme-color`, `mask-icon color`, `og:*` in `web/index.html`.
4. Change `version.Name`, the `<title>` and `common.appName` in all seven
   catalogs.
5. Keep the mark bidi-neutral: a glyph that only reads correctly left-to-right
   looks broken in `ar-SA` (the header mirrors with the document).
6. Rebuild the SPA and the Go binary (see [development.md](development.md)) —
   `web/dist` is embedded, and a stale binary hides every asset change.
