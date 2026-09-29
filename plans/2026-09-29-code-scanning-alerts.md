# Plan — GitHub Code Scanning remediation (SSRF guard + identifier conversions)

- **Date:** 2026-09-29
- **Repo:** https://github.com/ivancarlosti/sync
- **Goal:** clear the three open CodeQL alerts on `main` — one
  `go/request-forgery` (CWE-918) on the webhook notification sender and two
  `go/incorrect-integer-conversion` (CWE-681) on the identifier parameters of the
  API — without changing what an operator can configure.
- **Related:** same session as
  [`2026-09-29-dependabot-vulnerabilities.md`](./2026-09-29-dependabot-vulnerabilities.md);
  this one touches application code, that one only dependencies.

> Progress log rule: **update this file at the end of every work session / context loss point.**
> “Current status” must always describe exactly what is done and what is next.

---

## 0. Environment facts discovered

| Item | Value |
|---|---|
| Host Go | `go1.27.1` at `$HOME/.local/go/bin/go` (not on the bare `PATH`) |
| `gh` CLI | installed but **unauthenticated** → the alert list cannot be read from the API; the three findings below were identified by construction (the only tainted request URL and the only narrowing integer casts in the tree) |
| CodeQL rules | semantics confirmed against the upstream `.qll` sources: `RequestForgeryCustomizations.qll` (the barrier must be a check on the same value reaching `http.NewRequest*`, **in the same function**; a helper does not clear it), `RegexpCheck.qll` (a `Regexp.MustCompile` match used as a guard qualifies), `IncorrectIntegerConversionLib.qll` + `Strconv.qll` (`ParseUint(x, 10, 64)` → `uint` is a *possibly truncating* conversion wherever `uint` may be narrower than 64 bits) |
| CI | no workflow runs on pull requests in this repo → all verification below is local |

## 1. Findings

| # | Rule | Site | Why it fires |
|---|---|---|---|
| 1 | `go/request-forgery` (CWE-918) | `internal/notify/sender_webhook.go` — the channel `url` (stored in `notification_channels`, read back by the delivery worker) is passed to `http.NewRequestWithContext`; the only barrier was `strings.HasPrefix(target, "http://")` | the prefix check constrains nothing: `http://127.0.0.1:3000/…`, `http://169.254.169.254/…` and `http://user@10.0.0.5/…` all pass it, so a channel can address the instance itself, another container or the cloud metadata endpoint |
| 2 | `go/incorrect-integer-conversion` (CWE-681) | `internal/handlers/respond.go` `parseID` — `strconv.ParseUint(raw, 10, 64)` followed by `uint(value)` | a 64-bit parse narrowed to `uint` wraps on a 32-bit build (e.g. `4294967296` → `0`) |
| 3 | `go/incorrect-integer-conversion` (CWE-681) | `internal/handlers/respond.go` `queryID` — same pattern | same |

These are the **only** sites in the repository with a tainted request URL and the
only two narrowing casts of a parsed integer; every other outbound URL is a
constant or issued by a provider SDK, and the Go integer ingest path elsewhere
uses `Atoi` into `int`.

## 2. Decision — Option 2: strict shape **and** connect-time destination check

| Option | Verdict |
|---|---|
| 1. Shape only (a regexp guard) | clears the alert but leaves a stored channel able to reach loopback/metadata; rejected |
| **2. Shape guard + non-public destination refusal at connect time, with an operator opt-in** | **chosen**: it closes the alert *and* the real exposure (the worker runs inside the Sync container), while keeping LAN delivery possible through `NOTIFY_ALLOW_PRIVATE_TARGETS=true` |
| 3. Block everything non-public with no opt-in | breaks the legitimate “notify my Gotify on the LAN” use case; rejected |

Two properties of the fix were non-negotiable:

1. **The regexp guard must stay inline in the function that builds the request.**
   Moving it into a helper (`if !validURL(target)`) would put the barrier on the
   callee's parameter and the alert would remain. Only a *save-time* check would
   also fail: the taint flow starts at the database read, not at the API request.
2. **Resolve once, check every answer, dial the checked address.** Checking a
   name and then letting the transport resolve again is a DNS rebinding hole, so
   the dialer classifies the `LookupIPAddr` answer and connects to that exact
   address.

## 3. Changes

| File | Change |
|---|---|
| `internal/notify/egress.go` | **new** — `ErrPrivateTarget`, `reservedTargetBlocks` (CGNAT `100.64.0.0/10`, `192.0.0.0/24`, TEST-NET 1/2/3, benchmarking `198.18.0.0/15`, `240.0.0.0/4`, NAT64 `64:ff9b::/96`, documentation `2001:db8::/32`), `isPublicTarget`, `validateWebhookURLShape`, `newWebhookTransport` (resolving dialer, `Proxy: http.ProxyFromEnvironment`) |
| `internal/notify/sender_webhook.go` | `webhookURLPattern` + inline `MatchString` guard on the exact value passed to `http.NewRequestWithContext`; `maxWebhookRedirects = 5` and `guardRedirect` (drops `X-Sync-Signature` and every configured header on a host change, so a redirect cannot forward an API key); `NewWebhookSender(timeout, Options)` |
| `internal/notify/notify.go` | `Options{AllowPrivateTargets bool}`; `NewDispatcher(timeout, options)`; `Validate` now applies the same policy as a send, so a channel that could never deliver is refused before it is saved |
| `internal/config/config.go` | `NotifyAllowPrivateTargets` (`NOTIFY_ALLOW_PRIVATE_TARGETS`, default `false`) |
| `cmd/server/main.go` | passes the option into `notify.NewDispatcher` |
| `internal/handlers/respond.go` | `parseID` / `queryID` parse with `strconv.IntSize` instead of `64`, so the cast cannot truncate on any architecture (`strconv.IntSize` is exactly the width of `uint`) |
| `internal/handlers/notifications.go` | the webhook form hint now reads “absolute URL with a host, e.g. https://hooks.example.com/sync” instead of “http:// or https://” |
| `internal/notify/egress_test.go` | **new** — 29-case `isPublicTarget` table (incl. IPv4-mapped forms), connect-time refusal test (the request must not reach a loopback `httptest` server, and must reach it with the opt-in), redirect test (secrets dropped on a host change, loops bounded), unresolvable-name failure |
| `internal/notify/notify_test.go` | `TestWebhookRequestValidation` grown to 23 subtests (16 refusals incl. `ftp://`, `//host/hook`, `http://`, userinfo, port `99999`, embedded newline, malformed host; 7 accepted shapes incl. an IPv6 literal with port, query and fragment) plus the private-target policy loop |
| `internal/handlers/handlers_test.go` | new `TestIdentifierParametersCoverTheWholeRange` (400 for `abc`/`0`/`-1`/`0x10`/`1.5`/2^64, 404 for a valid unknown id, path **and** `job_id` query, width-aware expectations); the fixtures now pass `AllowPrivateTargets: true` because they deliver to loopback `httptest` servers |
| `internal/services/notify_test.go` | updated call site (new signature) |
| `docker/.env.example`, `docs/configuration.md`, `docs/notifications.md` | the new variable, its default and rationale; the `url` shape rules; the destination rules; the `HTTP(S)_PROXY` and shoutrrr caveats; troubleshooting rows |

Behaviour for a correctly configured deployment is unchanged: a public `https`
endpoint behaves exactly as before, and the reference platforms are 64-bit
(`linux/amd64`, `linux/arm64`), where `strconv.IntSize == 64` keeps the accepted
identifier range identical.

## 4. Verification

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `go test ./... -count=1` | ok — 7 packages green (`config`, `handlers`, `notify`, `providers`, `providers/google`, `providers/microsoft`, `services`) |
| `go test -race -count=1 ./internal/notify/... ./internal/handlers/...` | ok — no data race reported (the copied `http.Client` per delivery is what this checks) |
| new/changed notify tests, `-v` | 55 passing subtests (`isPublicTarget` 29, refusal 4, redirect 2, validation 23) |
| URL pattern | 23-case table, plus the 27 candidate strings validated during planning → 0 mismatches |
| alert closure | not executed here: `gh` is unauthenticated and the CodeQL CLI is not installed; closure follows from the barrier semantics of §0 (an inline regexp on the request URL) and from the two `strconv.IntSize` parses |

## 5. Current status

- **Done:** all three findings fixed, tested and documented; `gofmt`/`build`/`vet`/`test` green.
- **Deliberately left:** the `shoutrrr` kind (including the `smtp` host) is handed
  to the shoutrrr engine, which builds its own HTTP client, so the resolving
  dialer does not cover it — CodeQL flagged only the native webhook. A LAN mail
  relay or `generic://` target therefore still works, documented as a caveat
  instead of changed: a private SMTP relay is a legitimate setup and shoutrrr
  offers no hook for a custom client.
- **Next (optional):** validate a shoutrrr operator URL at save time (shape +
  literal-address check with the same `Options`), run the CodeQL CLI locally on
  before/after to record the alert transition, and add a CI workflow so a future
  finding is caught on the pull request instead of on `main`.

