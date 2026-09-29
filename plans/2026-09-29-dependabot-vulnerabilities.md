# Plan — Dependabot remediation (npm build chain + Go modules)

- **Date:** 2026-09-29
- **Repo:** https://github.com/ivancarlosti/sync
- **Goal:** clear every Dependabot alert open on `main` — the dev-only npm chain
  (vite / postcss / esbuild) and the Go module advisories — with no application
  behaviour change.
- **Related:** this supersedes the toolchain table of
  [`2026-09-27-first-building.md`](./2026-09-27-first-building.md), which pinned
  `vite 5.4.11` explicitly “for Node 18 compatibility”.

> Progress log rule: **update this file at the end of every work session / context loss point.**
> “Current status” must always describe exactly what is done and what is next.

---

## 0. Environment facts discovered

| Item | Value |
|---|---|
| Host Node | **18.19.1** — below Vite 8's floor, so every install/build was executed inside `node:22-alpine` (Node 22.23.3, npm 10.9.9), the same image the Dockerfile uses |
| Host Go | `go1.27.1` at `$HOME/.local/go/bin/go`; `govulncheck` at `$HOME/go/bin/govulncheck` (neither on the bare `PATH`) |
| Docker | 29.8.1, daemon reachable — used for the frontend build and the end-to-end image build |
| Dependabot | alerts are not readable without an authenticated GitHub token; the findings below were reconstructed with `npm audit` against the old lockfile and `govulncheck` against `go.mod` |
| CI | no workflow runs on pull requests in this repo → all verification below is local |

## 1. Findings

### npm (dev/build chain only — none of it is embedded in the binary)

`npm audit` on `web/package-lock.json` @ `ba34dca`: **3 vulnerable packages (2 high, 1 moderate)**,
i.e. 17 advisories in total.

| Package | Was | Advisories | Resolved by |
|---|---|---|---|
| vite | 5.4.11 | 12, range `<=6.4.2` (e.g. `GHSA-vg6x-rcgg-rjx6`, `GHSA-x574-m823-4x7w`, `GHSA-356w-63v5-8wf4`, `GHSA-859w-5945-r5v3`) | 8.3.1 |
| postcss | 8.4.49 | 4, range `<=8.5.22` (`GHSA-qx2v-qp2m-jg93`, `GHSA-6g55-p6wh-862q`, `GHSA-fxqj-rqcc-2cmp`, `GHSA-r28c-9q8g-f849`) | 8.5.28 |
| esbuild | 0.21.5 (transitive, via vite) | 1, range `<=0.24.2` (`GHSA-67mh-4wv8-2f99`, dev-server request leak) | gone — Vite 8 bundles with rolldown |

### Go

| Module | Was | Advisory | Fixed in |
|---|---|---|---|
| github.com/quic-go/quic-go | v0.59.0 | `GO-2026-5676` — HTTP/3 QPACK trailer expansion memory exhaustion, **reachable** | v0.59.1 |
| golang.org/x/net | v0.53.0 | `GO-2026-5026` — idna fails to reject ASCII-only punycode labels, **reachable** | v0.55.0 |
| golang.org/x/crypto | v0.50.0 | `GO-2026-5932` — `openpgp`, **no upstream fix**; that package is not imported | N/A |
| golang.org/x/sys | v0.43.0 | Dependabot alert (no reachable call path) | v0.44.0 |
| filippo.io/edwards25519 | v1.1.0 | Dependabot alert (indirect, via the MySQL driver) | v1.1.1 |

`govulncheck ./...` before: **2 reachable** + 1 in imported packages + 24 in required modules.

## 2. Decision — variant B (accept Vite 8)

The alternatives were to stay on Vite 5/6/7 and patch inside the major, or to take the
Dependabot `vite@8` PR. **Vite 8 was chosen**: it clears all 12 Vite advisories at once and
matches upstream mainline. Consequences accepted:

- the bundler is now **rolldown (+ lightningcss)** instead of rollup + esbuild → a large
  lockfile diff and no more `esbuild`/`rollup` in the tree;
- `vite@8` requires **Node `^20.19.0 || >=22.12.0`** → `web/package.json` → `engines.node`
  raised accordingly and the toolchain table in `docs/development.md` updated;
- `@vitejs/plugin-vue` 5.2.1 → **6.0.9** and `@types/node` 22.10.2 → **22.20.4**, both forced
  by Vite 8 peer ranges (its `@types/node` peer is `^20.19.0 || >=22.12.0`, so the previous
  22.10.2 pin made `npm install` fail with `ERESOLVE`).

## 3. Changes

| File | Change |
|---|---|
| `web/package.json` | `vite 8.3.1`, `postcss 8.5.28`, `@vitejs/plugin-vue 6.0.9`, `@types/node 22.20.4`, `engines.node ^20.19.0 \|\| >=22.12.0` |
| `web/package-lock.json` | regenerated with npm 10.9.9 inside `node:22-alpine` (`lockfileVersion 3`) |
| `docs/development.md` | Node row now states the Vite 8 floor |
| `go.mod`, `go.sum` | `quic-go v0.59.1`, `x/net v0.59.0`, `x/crypto v0.57.0`, `x/sys v0.48.0`, `edwards25519 v1.1.1` |

No application code, handler, config or Dockerfile change was needed. The `x/` modules were
taken at `@latest` because they are released in lockstep — `x/crypto@v0.57.0` requires
`x/net@v0.57.0`, which requires `x/sys@v0.46.0` — so pinning just the security floor would have
been inconsistent by construction.

## 4. Verification (all local; Node work inside `node:22-alpine`)

| Check | Result |
|---|---|
| `npm ci` (fresh install from the new lockfile) | 187 packages, **0 vulnerabilities** |
| `npm audit` | **0** (was 3 vulnerable packages / 17 advisories) |
| `npm run build` (i18n check + `vue-tsc` ×2 + `vite build` + postbuild) | ok — 2662 modules, built in 4.07 s, `dist/` 924 KB, `index-*.js` 275.65 kB (gzip 87.35 kB) |
| `go build ./... && go vet ./... && go test ./...` | ok — every package passes |
| `govulncheck ./...` | **No vulnerabilities found** — 0 reachable, 0 in imported packages |
| `docker build .` | ok — frontend stage (`npm ci` + build) and Go stage both green, image produced |

## 5. Current status

- **Done:** every npm and Go finding listed in §1 is fixed and verified; nothing reachable
  remains.
- **Deliberately left:** `GO-2026-5932` — no upstream fix exists and `openpgp` is not imported,
  so nothing is exposed; it disappears on its own with the next `x/crypto` release.
- **Next (optional):** add `.github/dependabot.yml` (grouped weekly npm + gomod updates) and a
  pull-request-time `govulncheck` / `npm audit` workflow, since no CI runs on PRs today; close
  the now-superseded Dependabot PR once this lands on `main`.
