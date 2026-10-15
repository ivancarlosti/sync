# Plan — npm advisories (`postcss-selector-parser`, `source-map-js`) + Tailwind 4

- **Date:** 2026-10-15
- **Repo:** https://github.com/ivancarlosti/sync
- **Goal:** clear the open npm advisories on `main` — `postcss-selector-parser`
  (fix 7.1.6) and `source-map-js` (fix 1.2.2), both **transitive** in `web/` —
  and, because those two sat under Tailwind 3, review every other dependency for
  safe upgrades and remove what is no longer used.
- **Related:** follows [`2026-09-29-dependabot-vulnerabilities.md`](./2026-09-29-dependabot-vulnerabilities.md),
  which took Vite 8 / PostCSS 8.5 and the Go advisories. This file supersedes its
  toolchain rows.

> Progress log rule: **update this file at the end of every work session / context loss point.**
> “Current status” must always describe exactly what is done and what is next.

---

## 0. Environment facts discovered

| Item | Value |
|---|---|
| Host Node | **v24.21.0**, npm **11.19.0** — inside `engines.node` (`^20.19.0 \|\| >=22.12.0`) |
| Container Node | `node:22-alpine` = Node **v22.23.3**, npm **10.9.9** — the lockfile was regenerated here (Dockerfile stage 1 / CI parity) |
| Host Go | **go1.27.1**, auto-downloads `go1.27.2` for the new `toolchain` line |
| Docker | 29.9.1 — used for the container install/build and for the visual gate |
| Visual gate | the repo's own `npm run screenshots` needs a running instance, so a real binary was run against a `mariadb:11.4` container. A **fresh DB has no run rows**, so the 18-step gate stops at `08-run-detail`; screens 01–07 were compared |

## 1. Findings

Both advisories are **build-time only** — nothing from `web/node_modules` is
embedded in the Go binary, and neither package is reachable at runtime.

| Package | Was | Came in via | Fix |
|---|---|---|---|
| `postcss-selector-parser` | 6.1.4 | `tailwindcss@3.4.17` → `postcss-selector-parser`, and `postcss-nested@6.2.0` | advisory wants **7.1.6** |
| `source-map-js` | 1.2.1 | `postcss`, `@vue/compiler-core` / `compiler-sfc`, `@intlify/message-compiler` | advisory wants **1.2.2** |

`npm audit` on the starting lockfile: **8 advisories**.

Why the obvious fix was not enough:

- **Both packages are pinned by several different parents at once**
  (`postcss-selector-parser` by `tailwindcss` and `postcss-nested`; `source-map-js`
  by `postcss`, `@vue/compiler-core`, `@vue/compiler-sfc` and
  `@intlify/message-compiler`), so bumping any one of them only moves part of the
  tree — `overrides` is the only lever that covers every edge at once.
- **`postcss-selector-parser@7.1.6` is still CJS-compatible** (no `"type"`, no
  `"exports"`), and its parse output is byte-identical for the selector grammar
  `tailwindcss`/`postcss-nested` use, so the major bump is safe as an override.
- **`tailwindcss@3.4.17` is itself unpatched**: it depends on the older
  `postcss-selector-parser` *and* on `braces`/`micromatch`/`fast-glob`/`chokidar`,
  and `braces` has **no patched release** (3.0.3 is the latest and still
  vulnerable). npm's only route to zero advisories is the `tailwindcss@4` major.
- **`tailwindcss@4.3.3` has zero runtime dependencies** and its Vite/PostCSS
  plugins pull no `chokidar` / `fast-glob` / `micromatch` / `braces` — so moving
  to v4 deletes the vulnerable subtree instead of patching around it.

## 2. Decision — `overrides` for the advisories, Tailwind 4 for the rest

Two independent moves, landed so that one can be reverted without touching the other:

**A. `overrides` (the security fix).** `web/package.json` gains a top-level
`overrides` block pinning `postcss-selector-parser@7.1.6` and `source-map-js@1.2.2`.
This is a one-file change that cannot alter application behaviour, and it is what
the advisory actually asks for. After the Tailwind 4 move, `postcss-selector-parser`
**disappears from the tree entirely** (its only parents were Tailwind 3 and
`postcss-nested`); the override stays anyway so any future transitive edge
re-introducing it is pinned above the advisory.

**B. Tailwind 4 (the residual cleanup).** `tailwindcss 3.4.17 → 4.3.3` is the only
way to reach zero advisories, because `braces` (a Tailwind 3 dev dependency) has no
patched release. Consequences accepted:

- Tailwind 4 is **CSS-first**: `tailwind.config.js`, `postcss.config.js`,
  `autoprefixer` and the JS `tailwindcss-animate` plugin go away, replaced by
  `@theme` / `@custom-variant` / `@plugin`-style directives inside `src/style.css`
  and the `@tailwindcss/vite` plugin;
- `tailwindcss-animate` → `tw-animate-css` (the maintained v4 replacement, a plain
  CSS file, so it is `@import`ed and tree-shaken);
- `tailwind-merge 2 → 3`, which is **required**, not optional: v2's default config
  knows only Tailwind 3 class names, so `cn()` would stop resolving conflicts on
  v4-only utilities such as `size-*`;
- browser floor moves to Safari 16.4+ / Chrome 111+ / Firefox 128+.

## 3. Changes

### npm — `web/package.json`

| Package | From | To | Why |
|---|---|---|---|
| `tailwindcss` | 3.4.17 | **4.3.3** | clears `braces`/`postcss-selector-parser`/`fast-glob`/`micromatch`/`chokidar` entirely |
| `@tailwindcss/vite` | — | **4.3.3** | v4's Vite plugin (replaces the PostCSS pipeline) |
| `tw-animate-css` | — | **1.4.0** | replaces `tailwindcss-animate` |
| `tailwind-merge` | 2.6.0 | **3.7.0** | v3 default config understands Tailwind 4 utilities |
| `pinia` | 2.3.0 | **4.1.0** | ESM-only release; the app already is ESM |
| `vue-router` | 4.5.0 | **5.4.0** | upstream states **no breaking changes** (it merges `unplugin-vue-router`) |
| `vue-tsc` | 2.2.0 | **3.3.13** | required by the TypeScript bump |
| `typescript` | 5.7.2 | **6.0.3** | last release that still exposes `./lib/tsc` |
| `vue` | 3.5.43 | 3.5.44 | patch |
| `vue-i18n` | 11.3.0 | 11.4.14 | minor |
| `vite` | 8.3.1 | 8.3.4 | patch |
| `@vitejs/plugin-vue` | 6.0.9 | 6.0.10 | patch |
| `reka-ui` | 2.10.5 | 2.11.0 | minor |
| `@lucide/vue` | 1.48.0 | 1.57.0 | minor |
| `@types/node` | 22.20.4 | 22.20.5 | patch, **stays on 22.x** on purpose |
| `autoprefixer` | 10.4.20 | **removed** | Tailwind 4 prefixes via Lightning CSS |
| `postcss` | 8.5.28 | **removed** | only `postcss.config.js` needed it directly |
| `tailwindcss-animate` | 1.0.7 | **removed** | superseded by `tw-animate-css` |
| — | — | `overrides` | `postcss-selector-parser: 7.1.6`, `source-map-js: 1.2.2` |

Two upgrades were **deliberately declined** and are the only entries left in
`npm outdated`:

- **`typescript` 7.0.2** — TypeScript 7 is the native (Go) compiler and its
  `exports` map no longer defines `./lib/tsc`, which is exactly the subpath
  `vue-tsc` resolves to locate `tsc`. Verified failure on 7.0.2:
  `Error [ERR_PACKAGE_PATH_NOT_EXPORTED]: Package subpath './lib/tsc' is not defined`.
  `6.0.3` is the newest release that works with `vue-tsc@3`.
- **`@types/node` 26.6.5** — pinned to the **Node 22** runtime the Dockerfile and
  CI actually use, so the types cannot advertise APIs the runtime lacks.

### Other files

| File | Change |
|---|---|
| `web/package-lock.json` | regenerated with **npm 10.9.9 inside `node:22-alpine`** (`lockfileVersion 3`) |
| `web/src/style.css` | Tailwind 4 CSS-first rewrite: `@import "tailwindcss"` + `@import "tw-animate-css"`, `@custom-variant dark/rtl/ltr`, `@theme inline` carrying the old `tailwind.config.js` theme (colours, radii, font stacks) |
| `web/vite.config.ts` | `plugins: [vue(), tailwindcss()]` + the `@tailwindcss/vite` import |
| `web/components.json` | dropped the `tailwind.config` key (no config file exists) |
| `web/tsconfig.json` | dropped `baseUrl`, which TypeScript 6 now rejects (`TS5101`); `paths` already resolves relative to the config file |
| `web/tailwind.config.js` | **deleted** — superseded by `@theme` |
| `web/postcss.config.js` | **deleted** — superseded by the Vite plugin |
| `web/src/assets/logo.svg`, `logo-icon.svg` | **deleted** — imported by nothing; `logo-icon.svg` documented a `BrandLogo.vue` that does not exist. `public/logo.svg` is the live mark |
| `docs/branding.md` | asset table and §3 theme-token text updated (Tailwind tokens now come from `@theme`) |
| `docs/development.md` | toolchain table: Go row, new Frontend row |

No application code (`src/views`, `src/components`, `src/stores`, `src/lib`,
`src/i18n`) needed a single edit, and neither did any Go handler or config.

## 4. Tailwind 4 behaviour deltas, checked one by one

| v4 change | How this codebase is affected |
|---|---|
| dark mode defaults to `prefers-color-scheme` | redefined with `@custom-variant dark (&:where(.dark, .dark *))` to keep the `.dark` class contract of `src/stores/theme.ts` |
| `rtl:`/`ltr:` are not built in | redefined as `@custom-variant`; verified `rtl:-translate-x-4` compiles to `.rtl\:-translate-x-4:where([dir=rtl],[dir=rtl] *)` |
| default border colour `gray-200` → `currentColor` | already overridden by `* { @apply border-border }`, which was kept |
| default `ring` width 3px → 1px | the app always states `ring-2` explicitly, so nothing changed |
| `outline-none` now means `outline-style: none` | used only inside the `:focus-visible` rule that also paints `ring-2`; visually identical |
| `peer-disabled:` compiles to `:is(:where(.peer):disabled ~ *)` | semantically identical to v3's `.peer:disabled ~ …`; confirmed in the CSS |
| preflight adds `::placeholder { color: color-mix(in oklab, currentcolor 50%, transparent) }` | every input sets `placeholder:text-muted-foreground`, which wins |
| the accordion `keyframes`/`animation` theme entries of `tailwind.config.js` | **dropped as dead code** — no `Accordion` component exists and `reka-ui` is only used for Switch / Tabs / Dialog / DropdownMenu |

## 5. Verification (all local)

### npm

| Check | Result |
|---|---|
| `npm install` + `npm audit` inside `node:22-alpine` | **0 vulnerabilities** |
| `npm ci` inside `node:22-alpine` (Dockerfile parity) | 160 packages, **0 vulnerabilities** |
| `npm run build` (i18n check + `vue-tsc` ×2 + `vite build` + postbuild) | ok — 2739 modules, built in **914 ms**; largest chunk `index-*.js` 305.26 kB (gzip 93.76 kB); `dist/` 1.1 MB |
| `postcss-selector-parser` in the tree | **absent** (`npm ls` reports nothing to resolve) |
| `source-map-js` | **1.2.2 at every edge** — `postcss@8.5.30`, `@vue/compiler-sfc@3.5.44`, `@intlify/message-compiler@11.4.14`, `@tailwindcss/node@4.3.3` |
| `npm outdated` | only `typescript` and `@types/node`, both intentional (§3) |

**Selector-level regression diff.** Every class selector was extracted from the
baseline (Tailwind 3) and the new (Tailwind 4) CSS and compared: **269 → 271**, the
only difference being the `peer-disabled:` composition above. Nothing was silently
dropped.

**CSS size.** 24 269 B → 36 924 B (gzip 5 486 B → 7 049 B). The growth is Tailwind
4's `@layer properties` / `@property` block (68 declarations) plus
`tw-animate-css` — not extra utilities, and the rule count tracks the selector
count (488 rules for 271 selectors).

**Determinism.** A clean build on the host and a clean build inside
`node:22-alpine` produce a **byte-identical** stylesheet (`md5 20cd48da0d81808451833d231036151c`).

**One caveat worth writing down.** Tailwind 4 discovers its sources by walking the
project root and applying `.gitignore`. Mounting **only `web/`** into a container
(`docker run -v …/web:/app`, without the repo root) hides the root `.gitignore`,
whose `web/dist/*` rule is the only thing stopping Tailwind from reading a previous
build's stylesheet as source. Reproduced deliberately:

| Build | planted `dist/assets/*.css` | result |
|---|---|---|
| host (repo root visible, `.gitignore` applied) | yes | 36 924 B, 488 rules |
| `node:22-alpine`, `web/` mounted alone | yes | **47 212 B, 592 rules** — 90 unused utilities such as `.blur`, `.capitalize`, `.border-l` |

The **real** Docker build is immune twice over: `.dockerignore` excludes
`web/dist` from the context, and stage 1 runs `rm -rf dist` before `npm run build`.
The warning is therefore only for anyone building inside a hand-rolled
`web/`-only container (and `npm run build` on the host is unaffected).

Note that `vite.config.ts` already sets `build.emptyOutDir: true` “so a stale asset
from a previous build is not embedded into the binary” — that guard empties the
*output*, but it does **not** run early enough to keep Tailwind from reading those
files during source discovery, which is why the feedback loop still reproduced.

**Visual A/B.** The real Go binary was built twice — once embedding the old
`dist`, once the new — and both were run against the same `mariadb:11.4` database,
then captured with the repo's own `npm run screenshots` (same 7 screens, same
data):

| Screen | pixels differing >16/255 |
|---|---|
| `01-login` | 0.061 % |
| `02-dashboard` | 0.194 % |
| `03-accounts` | 0.064 % |
| `04-jobs` | 0.064 % |
| `05-job-new` | 0.162 % |
| `06-job-editor` | 0.162 % |
| `07-runs` | 0.064 % |

No size changes, no layout shift in any screenshot, and side-by-side crops of the
login card and the dashboard stat grid are indistinguishable. The residual
sub-0.2 % is sub-pixel antialiasing.

### Go (found while reviewing “other modules that can be upgraded”)

`govulncheck ./...` flagged **9 reachable vulnerabilities**, all in HTTP/2 /
TLS / textproto — i.e. in the Go **standard library** and in `golang.org/x/net`,
none of them in this repo's own code:

| Vulnerability | Module | Fix |
|---|---|---|
| `GO-2026-6603` | `x/net@v0.59.0` + `net/http/internal/http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |
| `GO-2026-6605` | `net/http@go1.27.1` | `go1.27.2` |
| `GO-2026-6607` | `crypto/tls@go1.27.1` | `go1.27.2` |
| `GO-2026-6608` | `net/textproto@go1.27.1` | `go1.27.2` |
| `GO-2026-6610` | `x/net@v0.59.0` + `http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |
| `GO-2026-6611` | `x/net@v0.59.0` + `http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |
| `GO-2026-6612` | `x/net@v0.59.0` + `http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |
| `GO-2026-6613` | `x/net@v0.59.0` + `http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |
| `GO-2026-6617` | `x/net@v0.59.0` + `http2@go1.27.1` | `x/net@v0.60.0` / `go1.27.2` |

Because the standard library is linked into the binary, patching only `x/net`
would not have been enough — the **toolchain** had to move too:

| File | Change |
|---|---|
| `go.mod` | `toolchain go1.27.2` (with a comment explaining why), `x/net v0.59.0 → v0.61.0`, `x/crypto v0.57.0 → v0.58.0`, `x/sys v0.48.0 → v0.49.0`, `x/text v0.42.0 → v0.43.0`, `kiota-http-go v1.5.6 → v1.5.7`, `msgraph-sdk-go v1.103.0 → v1.104.0` |
| `go.sum` | regenerated |
| `Dockerfile` | `golang:1.27-alpine` → **`golang:1.27.2-alpine`**, so release images are built with the patched stdlib |

The two direct Microsoft updates were the only non-transitive bumps available;
every other direct requirement (`gin`, `shoutrrr`, `godotenv`, `gorm`,
`kiota-*`, `msgraph-sdk-go-core`, `x/oauth2`) was already at its latest release.

| Check | Result |
|---|---|
| `go mod tidy -diff` | clean — no unused or missing requirement |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go test ./...` | ok — every package with tests passes |
| `govulncheck ./...` | **No vulnerabilities found** — 0 reachable, 0 in imported packages (was 9 reachable) |

## 6. Current status

- **Done and verified:** both named npm advisories are closed (`npm audit`: 8 → **0**),
  the Tailwind 3 subtree that carried them is gone, every other dependency was
  reviewed, three dev dependencies and four dead files were removed, and the Go
  side went from **9 reachable** vulnerabilities to **0**.
- **Deliberately left as-is, with reasons recorded in §3:** `typescript@7.0.2`
  (incompatible with `vue-tsc`) and `@types/node@26` (would not match the Node 22
  runtime pin). These are the only `npm outdated` entries that remain.
- **Not changed, on purpose:** no Go handler, config, model or migration was
  touched, and no application source in `web/src` was edited — the frontend diff
  is confined to build config, the stylesheet and deletions.
- **Next (optional):** add `.github/dependabot.yml` (grouped weekly npm + gomod
  updates) and a pull-request-time `npm audit` / `govulncheck` job, since no CI
  runs on pull requests in this repo; and re-run the full 18-screen screenshot
  gate on an instance whose database actually contains runs, so screens 08–18 get
  the same A/B treatment as 01–07.




