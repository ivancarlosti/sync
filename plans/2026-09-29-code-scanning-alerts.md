# Plan — GitHub Code Scanning remediation (SSRF guard + identifier conversions)

- **Date:** 2026-09-29
- **Repo:** https://github.com/ivancarlosti/sync
- **Goal:** clear the three open CodeQL alerts on `main` — one
  `go/request-forgery` (CWE-918) on the webhook notification sender and two
  `go/incorrect-integer-conversion` (CWE-681) on the identifier parameters of the
  API — without changing what an operator can configure. A **fourth** alert of
  the same CWE-918 family (a Drive identifier interpolated into a request URL by
  the Google provider) was found by re-running the rule locally on the fixed
  tree and is fixed in §3.1.
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
| 4 | `go/request-forgery` (CWE-918) | `internal/providers/google/drive.go` (`Children`, `Item`, `Download`, `Delete`) and `upload.go` (`Upload`, `CreateFolder`) — the item/folder/drive id is concatenated into the `https://www.googleapis.com/drive/v3/…` URL. Found after the fix of finding 1 made the rule re-run locally; the ids are tainted because they arrive from an explorer link (`:drive`, `folder_id`) and from the ids stored on a job/run | a crafted id (`../permissions`, `?alt=media`, `#…`, `files/…`, an encoded separator, an embedded newline) redirects the call to another Drive endpoint or another path on the host, with the account's own bearer token |

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
| `internal/providers/provider.go` | **new** `ErrInvalidIdentifier` sentinel (finding 4) |
| `internal/providers/google/identifiers.go` | **new** — `identifierPattern` (`^[A-Za-z0-9_-]{1,512}$`), `optionalIdentifierPattern` (empty allowed: the drive id position), `requestURLPattern` (`^https://([A-Za-z0-9-]+\.)+googleapis\.com/[url-safe]*$`) and the two error helpers wrapping `providers.ErrInvalidIdentifier` |
| `internal/providers/google/drive.go` | inline guards in `Children`, `Item`, `Download`, `Delete` — on the folder/item id and, when non-empty, on the drive id |
| `internal/providers/google/upload.go` | inline guards in `Upload` (item id when updating, parent folder, drive) and `CreateFolder` (parent folder, drive) |
| `internal/providers/google/identifiers_test.go` | **new** — refusal table (17 shapes × 13 call sites: every entry point refuses, and the recording transport proves no request was built) and an accept table (a real token still reaches `googleapis.com` with the id in place, plus the synthetic `my-drive`/`root` pair); a second half pins the request-URL layer (13 forbidden URLs — another host, a look-alike host, http, a port, userinfo, a relative URL, a backslash, a newline — are refused by `doJSON` without a request, and the six shapes the provider builds pass) |
| `internal/providers/google/client.go` | the inline `requestURLPattern` guard at the top of `doJSON`, the one function through which every JSON request is sent |
| `internal/providers/google/drive.go`, `upload.go` | the same inline guard on the download URL and on both legs of the upload (initiation URL, upload session URI from the `Location` header) |
| `internal/handlers/respond.go` | `statusFor`/`codeFor` map `providers.ErrInvalidIdentifier` → `400`/`validation` |
| `internal/handlers/handlers_test.go` | `TestAccountScreensRefreshAndFailures` also covers a provider identifier refusal answering 400 instead of 500 |
| `docs/api.md`, `docs/providers.md` | the `validation` meaning, the remote-identifier rule, the pattern and why the guard stays inline |
| `docker/.env.example`, `docs/configuration.md`, `docs/notifications.md` | the new variable, its default and rationale; the `url` shape rules; the destination rules; the `HTTP(S)_PROXY` and shoutrrr caveats; troubleshooting rows |

Behaviour for a correctly configured deployment is unchanged: a public `https`
endpoint behaves exactly as before, and the reference platforms are 64-bit
(`linux/amd64`, `linux/arm64`), where `strconv.IntSize == 64` keeps the accepted
identifier range identical.

## 3.1 Finding 4 — Google Drive identifiers (same CWE-918, second site)

The provider builds its request URLs by interpolation (`apiBase + "/files/" +
itemID`, `withParam(target, "driveId", driveID)`), and the ids come from outside:
the `:drive` path parameter and `folder_id` of the explorer endpoint, and the ids
stored on a job/run by the engine. A value such as `../permissions`,
`files/root`, `1AbCdEf?alt=media`, `1AbCdEf#frag`, `1AbCdEf%2F..`, `…@evil.example`
or a string with an embedded newline would therefore address a *different* Drive
endpoint than the caller asked for — with the account's own bearer token
attached.

| Option | Verdict |
|---|---|
| 1. Guard inside `withParam`/a shared helper | rejected: the analyser did not accept it (the guard has to sit on the value that is interpolated, in the function that builds the request — the same lesson as §2, point 1), and it would have covered only the query part, not the path segments |
| 2. `url.Parse` + `ResolveReference` with a host check | rejected: it changes every call site for no benefit — the host is a constant, the id is the only variable, and a Drive id can never contain a slash anyway |
| **3. Inline shape guard on every interpolated id, one pattern per position** | **chosen** |

The guard (`internal/providers/google/identifiers.go`) is:

* `identifierPattern = ^[A-Za-z0-9_-]{1,512}$` — the base64url alphabet Drive
  hands out, plus the synthetic ids Sync adds (`root`, `my-drive`). No path
  separator or `..`, no `?`/`#`/`@`/`:`/`\`, no percent-encoded form of one, no
  whitespace or control character (so an encoded newline cannot split a request
  line either), and a length bound far above a real token.
* `optionalIdentifierPattern = ^[A-Za-z0-9_-]{0,512}$` for the *drive* id
  position only, where `providers.Provider` documents an empty value as "the
  drive the account considers its default" (the other positions default to
  `DriveRoot` before the guard runs).
* A refusal returns `invalidIdentifierError(kind, value)`, wrapping
  `providers.ErrInvalidIdentifier`; `internal/handlers/respond.go` maps that to
  `400`/`validation` (§3) because the id is the caller's to fix.

The guard is **inline in each using function** — `Children`, `Item`, `Download`,
`Delete` in `drive.go` and `upload`/`CreateFolder` in `upload.go` — immediately
above the request it protects, so both the analyser and a reader see that the
value cannot leave the endpoint it belongs to. Two consequences worth stating:

* Nothing legitimate is narrowed: the pattern accepts every id Drive returns
  (opaque base64url tokens) and both synthetic ids, and the drive position still
  accepts empty. `identifiers_test.go` pins the accept side against a stub
  transport, including the `my-drive`/`root` pair.
* The refusal happens *before* a request is built (the client is created after
  the guard), which the refusal half of the same test asserts through a recording
  transport: for a refused value no request reaches the network at all.

### 3.1.1 The per-request layer (what actually cleared the alert)

Re-running the rule after the per-identifier guards still produced one result —
`client.go`, the `client.Do(req)` inside `doJSON`. The reason is the same lesson
as §2, point 1, seen from the other end: the guard sat on the *caller's*
argument, while the sink is reached through `doJSON`'s parameter, so the
sanitised value and the tainted value are two different data-flow nodes.

The fix is a second, coarser guard, applied to the finished URL at every request
site (three functions, four sites):

* `requestURLPattern = ^https://([A-Za-z0-9-]+\.)+googleapis\.com/[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]*$`
  — an absolute `https` URL on a subdomain of `googleapis.com` (Google's own
  domain, where the Drive API, the upload root and the resumable session URI all
  live), followed by a path/query/fragment drawn from the URL-safe alphabet.
* It is applied inline in `doJSON` (before the request is built) and at the three
  remaining sites that call `http.NewRequestWithContext` themselves — `Download`
  and the two legs of `upload`. The session URI is checked too, so a wrong
  `Location` from Google cannot post the bytes of a file elsewhere.
* Site 4 returns `unsafeRequestURLError`, which wraps the same
  `providers.ErrInvalidIdentifier`, so a URL that somehow bypasses the
  per-identifier guard still answers `400` with the offending value instead of a
  `500`. The session-URI case is a *provider* anomaly rather than caller input,
  so it returns a plain error (→ `500`).

The host pin is `*.googleapis.com` rather than the literal `www.googleapis.com`
on purpose: the alphabet and the subdomain rule are what make the value safe
(Google owns `googleapis.com`, and a label boundary plus the required `/` refuse
`www.googleapis.com.evil.example`, `notgoogleapis.com` and
`www.googleapis.com@evil.example`), while a literal host would have broken
uploads the day Google answers from another subdomain (e.g. a regional upload
host). Refusing a port (`googleapis.com:8443`) is intentional — the provider
never uses one.

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
| alert closure | **measured**, see below |
| `identifiers_test.go -run 'Refuses\|Accepts' -v` | `TestProviderRefusesValuesThatAreNotDriveIdentifiers`, `TestProviderAcceptsDriveIdentifiers`, `TestProviderRefusesRequestsOutsideTheDriveAPIHost` all pass |

### 4.1 CodeQL, run locally on the fixed tree

The CLI (`codeql-linux64.zip` unpacked to `/tmp/q/codeql`, pack
`codeql/go-queries@1.6.11` installed under `~/.codeql/packages`) was used on a
database built from the working tree, so the closure of every finding is measured
rather than deduced:

| Query | Finding 1 (webhook) + 4 (Drive ids) | Result |
|---|---|---|
| `Security/CWE-918/RequestForgery.ql` (before: the tree with findings 1–3 fixed) | 1 result, `internal/providers/google/client.go` — the `client.Do` inside `doJSON` | the per-identifier guards alone did **not** clear finding 4 ([§3.1.1](#311-the-per-request-layer-what-actually-cleared-the-alert)) |
| `Security/CWE-918/RequestForgery.ql` (after the per-request layer) | **0 results** on two databases (one before the final comment edits, one built from the tree as committed: `/tmp/db3`, `/tmp/db4` → `/tmp/q/after3.sarif`, `/tmp/q/final.sarif`) | findings 1 and 4 closed |
| `codeql-suites/go-security-and-quality.qls` (58 queries, the suite GitHub's default setup runs) | 13 results, all `go/log-injection` (CWE-117) on `middleware.go`, `notifications.go`, `services/auth.go`, `services/oauth.go` | no `go/request-forgery` and no `go/incorrect-integer-conversion` remain — findings 1–4 closed; the 13 log-injection results are pre-existing (none of those files is touched by this change) and out of this plan's scope |

## 5. Current status

- **Done:** all four findings fixed, tested and documented; `gofmt`/`build`/`vet`/`test`
  green; `go/request-forgery` and `go/incorrect-integer-conversion` verified closed
  with the real CodeQL engine on the working tree (§4.1).
- **Observed while verifying, not in scope:** 13 `go/log-injection` (CWE-117)
  results in the `security-and-quality` suite (request path, notification target,
  OAuth state/error text logged verbatim). They are a separate rule family, were
  not part of the reported alert list, and no file carrying them is touched here.
- **Deliberately left:** the `shoutrrr` kind (including the `smtp` host) is handed
  to the shoutrrr engine, which builds its own HTTP client, so the resolving
  dialer does not cover it — CodeQL flagged only the native webhook. A LAN mail
  relay or `generic://` target therefore still works, documented as a caveat
  instead of changed: a private SMTP relay is a legitimate setup and shoutrrr
  offers no hook for a custom client.
- **Next (optional):** sanitise the 13 log-injection sites, validate a shoutrrr
  operator URL at save time (shape + literal-address check with the same
  `Options`), and add a CI workflow running the CodeQL suite so a future finding
  is caught on the pull request instead of on `main`.

