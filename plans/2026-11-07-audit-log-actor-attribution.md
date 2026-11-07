# Plan — Audit log actor attribution (user vs. service, and who is really behind it)

Status: **implemented** (2026-11-07). Verification and the deviations from this
plan are recorded in §7.

## 1. Context

An operator reading the Audit Logs screen of a Microsoft 365 tenant sees rows
like this for a device registration:

```
Updated · Other · DESKTOP-J979VG0 · device registration service · … · microsoft · directory · ivan.almeida@bowler.com.br
```

and reads it as "`ivan.almeida@…` registered that device". Three things are wrong:

1. **The actor slot holds a service name, not an address.**
   `directoryActorEmail()` (`internal/providers/microsoft/activitylog.go:315`)
   falls back to `initiatedBy.app.servicePrincipalName` / `displayName` and
   lowercases it into `AuditLogEvent.ActorEmail`. The schema has no way to say
   "this event was made by a workload, not a person": `models.AuditLogEvent`
   (`internal/models/auditlog.go:38`) has one actor slot and no actor *kind*.
2. **The person is thrown away.** For `Add device` / `Update device`, Graph's
   `auditLogs/directoryAudits` reports
   `initiatedBy.app.displayName = "Device Registration Service"` and **no
   `initiatedBy.user`**: the enrolling human is only recoverable from
   `targetResources[].modifiedProperties` / `additionalDetails`, from a later
   ownership record (`Add registered owner to device`, which *does* carry
   `initiatedBy.user`), or from the device object's `registeredOwners`.
3. **The Organisation column is misread.** Its third line is `readers(event)`
   (`web/src/views/AuditLogsView.vue:428`), i.e. the connected accounts that
   *poll* the feed — the collector identity. Unlabelled, it looks like
   attribution, which is what made the row read as "ivan did this".
4. A device is also filed under the wrong object: `directoryTarget()`
   (`activitylog.go:370`) maps only `User` and `Group`, so
   `targetResources[].type = "Device"` becomes `target = other`.

### 1.1 Why not add `Device.Read.All`

The obvious fix — declare `Device.Read.All` in the permission table and read
`/devices/{id}/registeredOwners` — costs more than it looks:

- `oauthConfig()` builds the token source from `(&Provider{}).Scopes()`
  (`internal/providers/microsoft/provider.go:216`) and `Refresh()` reuses that
  config, so **every refresh asks Entra for the new scope**. An account whose
  refresh token was issued before the scope existed fails with
  `invalid_grant`/`AADSTS65001 consent_required`; `TokenManager.refresh`
  (`internal/services/tokens.go:214`) then marks it `ErrReconnect` and *every*
  capability of that account (files, users, groups, logs) stops until somebody
  reconnects it.
- Narrowing the refresh to the old scopes is not an escape either: this endpoint
  issues a token restricted to the requested scopes, so the enrichment would
  disappear on the first refresh of every account.

The read does not need a new scope: `registeredOwners` is readable with any of
`Device.Read.All`, `Device.ReadWrite.All`, `Directory.Read.All`,
`Directory.ReadWrite.All`, and **`Directory.ReadWrite.All` is already declared,
administrator-consented and requested** by this provider
(`internal/providers/microsoft/permissions.go:62`, capability `groups`). The
enrichment therefore rides on a grant the stored tokens already carry: no new
consent, no reconnect, no refresh regression. Least privilege (a read-only
`Device.Read.All`) stays available as a later, isolated change — it needs
`Provider.Refresh` to carry the stored grant so optional scopes are requested
only when they were already consented (see §6).

## 2. Goal

One row per event that answers "who did it" without lying:

- a **person** → e-mail + name, `actor_kind = user`;
- a **workload** → its display name, `actor_kind = application`, no e-mail in the
  e-mail column, plus (when it can be recovered) the human it acted for in
  `actor_on_behalf_of`;
- neither → `actor_kind = system`, named after `loggedByService`;
- the object is told apart (`device`, `application`, …) instead of `other`;
- the connected account that reads the feed is **labelled as such** in the
  Organisation column.

Scope: Microsoft 365. Google's normaliser already names `actor.email` from its
own feed and keeps working (its rows carry no `actor_kind`, which the screen
reads as a person).

## 3. Steps

### 3.1 Vocabulary and schema (additive)

| File | Change |
|---|---|
| `internal/models/enums.go` | `AuditLogTargetDevice = "device"`, `AuditLogTargetApplication = "application"` + `Valid()`; new `AuditLogActorKind` (`user`, `application`, `system`) with `Valid()`, written like `AuditLogAction` |
| `internal/models/auditlog.go` | `ActorKind` (`size:32;index`), `ActorOnBehalfOf` (`size:191;index`), `ActorOnBehalfOfName` (`size:191`) |
| `internal/providers/provider.go` | the same three fields on `ActivityEvent`, plus the optional interfaces `ActivityLogNormaliser` (re-read a stored vendor record) and `ActivityActorEnricher` (name the human behind a workload event) |

`AutoMigrate` adds the columns; no historic row is rewritten silently
(`docs/database.md` §5). Rows stored by an older release have `actor_kind = ''`,
which is the marker the repair pass of §3.4 looks for.

### 3.2 Microsoft normaliser

- `directoryActor(record)` replaces the three `directoryActor*` helpers and
  returns `(kind, email, name, id, onBehalfOf, onBehalfOfName)`:
  `initiatedBy.user` wins; else `initiatedBy.app` (kind `application`, **empty
  e-mail**, name = `displayName || servicePrincipalName`); else `system`
  (`loggedByService`).
- the record struct gains `targetResources[].modifiedProperties` and the
  top-level `additionalDetails`; `registeredOwnerFromRecord()` reads
  `RegisteredOwner`/`RegisteredUsers` (+ `DeviceId`) out of them, which is how an
  activity that *does* carry the person (`Add registered owner to device`) stops
  being indistinguishable from the rest.
- `directoryTarget()` maps `Device` → `device`, `Application` /
  `ServicePrincipal` → `application`.

### 3.3 Owner lookup (best effort, bounded)

New `internal/providers/microsoft/deviceowners.go`:

```
GET /v1.0/devices/{id}?$select=id,deviceId,displayName&$expand=registeredOwners($select=id,displayName,userPrincipalName)
```

`{id}` is the object id of the device resource, and Graph accepts the Entra device
id in the same position (which is what a record naming only `DeviceId` carries), so
one endpoint covers both shapes and no `$filter` fallback is needed.

- `EnrichActivityActors(ctx, tokens, events)` mutates the events of a batch: only
  workload actors on a `device` target without an in-record owner, memoised by
  device id, **at most 20 lookups per call**;
- gated on the scopes Graph accepts for that read (`Directory.ReadWrite.All`,
  `Directory.Read.All`, `Device.Read.All`, `Device.ReadWrite.All`), taken from the
  `scp` claim of the access token in hand with the stored grant as the fallback
  (`providers.GrantsScope`, added for this — a capability asks for *every*
  permission it declares, which is the wrong question for a read any of several
  scopes authorises). A missing grant degrades silently: the screen explains it
  (§3.5);
- anything else — 403, 404, throttle, gone device — is logged and skipped: the
  enrichment never fails a pass and never invents a name.

### 3.4 Storage, repair, service

- `auditLogRow()` carries the new fields; the `ActorEmail` slot is no longer
  filled from an application name.
- `RepairActors(ctx)` reparses the stored `raw` document of rows with
  `actor_kind = ''` through the provider's `ActivityLogNormaliser`, then runs
  `EnrichActivityActors` for the device rows among them, and writes the result
  back (actor columns and target). Idempotent by construction (`actor_kind` becomes
  non-empty), batched (500 rows, 20 batches, 2 minutes per pass), and run
  automatically at the end of every collect pass — so an upgrade repairs the
  history without anybody clicking anything — plus on demand through
  `POST /api/maintenance/auditlogs/renormalise`.
- `AuditLogFilter`: `actor` also matches `actor_on_behalf_of`; `search` also
  matches `actor_name` and `actor_on_behalf_of`, so a service actor stays
  findable once it is no longer an address.

### 3.5 API and screen

- `auditLogView` and the CSV carry `actor_kind`, `actor_on_behalf_of`,
  `actor_on_behalf_of_name`; the target list gains `device` and `application`.
- `AuditLogsView.vue`: kind-aware actor cell (a workload renders its name and a
  service chip, never an address; a person renders the address), an
  `on behalf of …` line, the reader line prefixed with a label, and a hint when
  the organisation's account cannot resolve owners (`groups` in
  `missing_capabilities`, data the screen already loads).
- i18n keys in **all seven** catalogs (`ar-SA`, `en-US`, `es-MX`, `fr-FR`,
  `h-CN`, `hi-IN`, `pt-BR`) — `web/scripts/check-i18n.mjs` enforces parity.

### 3.6 Docs and tests

`docs/audit-logs.md` (columns, "who is the actor?", the device-registration case,
the repair pass), `docs/api.md` (view fields, the maintenance row),
`docs/database.md` §3, `docs/app-registration.md` (no new scope, and why).
Tests: `activitylog_test.go`, new `deviceowners_test.go`, `store_test.go`,
`auditlog_test.go`, `handlers/auditlogs_test.go`, plus `check-i18n` and the SPA
build.

## 4. Risks

| Risk | Mitigation |
|---|---|
| The enrichment rides on a write-capable grant it never writes with | Documented in `docs/audit-logs.md` and `docs/app-registration.md`; reversible through the `Device.Read.All` follow-up of §6 |
| A tenant without `Directory.ReadWrite.All` gets no owner line | The screen says why (hint from `missing_capabilities`); the row keeps an honest service actor |
| Extra Graph calls per pass | Deduped by device, capped at 20, memoised, throttling handled by the existing retry policy |
| A device deleted from the tenant has no owner to resolve | The row keeps the service actor and no `on behalf of` line — no invented attribution |
| Legacy rows | The repair pass (`actor_kind = ''`) converges after one pass and is idempotent |
| New enum values live in four places | Same-step edits: Go enum + `Valid()`, handler vocabulary, TS union, i18n catalog |
| `check-i18n.mjs` / `vue-tsc` fail on a forgotten key | Run before committing |

## 5. Verification

```
go build ./... && go vet ./... && go test ./...
cd web && npm run typecheck && npm run build
```

Then the organisation security sweep (Dependabot PRs, Dependabot alerts,
code-scanning alerts, `gh run list --limit 5`) after the Actions/CodeQL run.

## 6. Deferred: least privilege (`Device.Read.All`)

If a tenant's security review refuses to read owners with a write-capable scope,
the swap is: `Permission.Optional` in the table, `Provider.Refresh` receiving the
stored `providers.Tokens` instead of only the refresh-token string so the request
asks for optional scopes **only when the stored grant already carries them**, and
`deviceowners.go` reading the read-only scope. That keeps existing connections
working (no `AADSTS65001`) while new connections consent to the narrower scope.

## 7. What shipped, and the deviations

| Planned | Shipped as |
|---|---|
| `actor_kind`, `actor_on_behalf_of(_name)` on the model and on `providers.ActivityEvent` | Yes; `ActivityEvent.ActorKind` is typed `models.AuditLogActorKind` (not a bare string), like `Action`/`Target` |
| `directoryActor()` returning a tuple | A `directoryPrincipal` value (`directoryPrincipalOf`), because six unnamed return values are unreadable at the call site |
| Target `device`/`application` | Yes, plus `Site` (Graph's `Site`) while the mapper was open |
| In-record owner recovery | Yes: sibling user resource → `RegisteredOwner`/`RegisteredUsers` → owner-like `additionalDetail`, address-shaped values only |
| Owner lookup in `deviceowners.go` | Yes, with the scope gate reading the token's `scp` claim (the stored grant is only the fallback — a refresh that returns no `scope` value would otherwise disable it) and `providers.GrantsScope` exported for it |
| `$filter=deviceId` fallback | Dropped: `/devices/{id}` accepts the Entra device id too, so one shape covers both |
| Repair pass | `RepairActors`: batches of 500, at most 20 batches **and 2 minutes** per pass (the pass runs inside the 10 minute collect tick), started at the end of every collect and reachable through `POST /api/maintenance/auditlogs/renormalise` |
| Repair rewrites the actor | The actor **and** the target: the same re-read classifies both, and leaving a device filed as `other` would have fixed only half of the reported row |
| Rows without `raw` | Repaired from their own columns: a value without `@` moves from `actor_email` to `actor_name` as an `application` actor, so a legacy service name cannot be re-read as a person |

Verification: `gofmt`, `go build ./...`, `go vet ./...` and `go test ./...` clean;
`cd web && npm run typecheck && npm run build` clean (`check-i18n`: 7 catalogs ×
739 keys). New tests: `directoryPrincipal`/target/owner recovery and the normaliser
contract (`activitylog_test.go`), the owner lookup, its grant gate, its bound and its
failure tolerance (`deviceowners_test.go`), the filter and repair storage
(`auditlog_store_test.go`), the repair pass and its wiring into `Collect`
(`auditlog_test.go`), the API answer and the maintenance endpoint
(`auditlogs_test.go`). Open Dependabot PRs, Dependabot alerts and code-scanning
alerts: none at the time of writing.

Still to do by hand: regenerate `docs/screenshots/10-audit-logs.png` against a live
instance with a real Microsoft tenant (`cd web && npm run screenshots`), which no
offline build can produce — the committed image predates the kind-aware actor cell.
