# Activity feed failures: Google manifest, Microsoft consent and throttling

## Context

The audit-log pass (`AuditLogService.Collect`, one pass every
`DefaultAuditLogInterval` = 10 minutes) has been failing on three independent
causes, none of them an invalid token:

1. **Google `drive` answers HTTP 400 with zero events.** The Reports API
   validates *every* `eventName` of the request against the published manifest of
   the application and rejects the **whole request** when one name is unknown.
   `driveEventNames` listed `change_acl_scope`, which does not exist in Google's
   Drive event reference, so the curated filter killed the entire drive stream.
   The valid sharing names include `change_user_access`,
   `change_document_access_scope`, `change_acl_editors`,
   `change_document_visibility` and `change_owner`.
2. **Microsoft `unified` answers HTTP 401 `AF10001` with `permission set ()`.**
   The app-only token for `manage.office.com` carries no roles: the app
   registration is missing the **application** permission `ActivityFeed.Read` on
   the Office 365 Management APIs and/or an administrator of that directory never
   granted consent (the grant is per directory). Unified audit logging must also
   be turned on in Purview, or the subscription succeeds and the blobs are empty.
3. **Microsoft `directory` answers HTTP 429 and is never retried.**
   `directoryAuditLogs` goes through `getJSON`, the only Microsoft call that does
   *not* use `providers.DoWithRetry`; `unified.go` (`appOnlyToken`,
   `startSubscription`, `contentBlobs`, `blobRecords`) has no retry either. The
   collector also stops the remaining streams of an organisation after a failure,
   so one throttled `directory` call costs both Microsoft feeds for 10 minutes.

## Goal

- The Google drive feed reads events again, with a filter made only of published
  event names, and an unknown name can no longer blind the feed.
- Both Microsoft feeds retry a throttle with the `Retry-After` the audit
  endpoints actually ask for.
- An `AF10001` failure explains itself and lands in the "needs consent" state of
  the Audit Logs screen instead of looking like a transient fault.
- The documentation tells an operator how to grant `ActivityFeed.Read` per
  directory and what each failure means.

## Steps

1. **Google, valid names** (`internal/providers/google/activitylog.go`): drop
   `change_acl_scope`, add `change_document_visibility` and `change_owner` to
   `driveEventNames`; document that the list is validated against the manifest
   and that one unknown name fails the whole request. Map both new names to
   `models.AuditLogShare` in `classifyActivity`.
2. **Google, self-healing filter**: `activityURL` takes the name list explicitly;
   `ActivityLogs` reads each page through a new `activityPage` helper that, on a
   `400 … Event <name> not found in manifest`, removes that name and retries the
   same page once — bounded by the number of names, and never looping on an error
   it cannot attribute to a name.
3. **Microsoft, retry** (`internal/providers/microsoft/activitylog.go`,
   `unified.go`): add `activityRetryPolicy` (`MaxAttempts: 3`,
   `MaxDelay: 90s` — the audit endpoints ask for a minute or more, the default
   30s cap retries too early, and the pass has `requestTimeout` = 5m) and a
   `doRequest` helper that replays a POST body from `GetBody`. Route `getJSON`,
   `appOnlyToken`, `startSubscription`, `contentBlobs` and `blobRecords` through
   it.
4. **Microsoft, explain AF10001**: append an actionable hint (application
   permission, per-directory admin consent, unified audit logging in Purview) to
   the subscription failure, and add `af10001` to the `needsConsent` markers of
   `internal/services/auditlog.go`.
5. **Tests**: pin the exact documented drive event set and the absence of
   `change_acl_scope`; cover the manifest trim (2 requests, second without the
   rejected name, no error); cover the directory feed retrying a `429` with
   `Retry-After: 0` and *not* retrying a `400`; cover the `AF10001` message.
6. **Docs**: `docs/audit-logs.md` troubleshooting section (manifest 400, AF10001,
   429 and the 10-minute backoff) and `docs/app-registration.md` §3.4 rewritten
   around the two failures actually observed.

## Risks

- The Microsoft consent fix is **operational**, not code: until an administrator
  grants `ActivityFeed.Read` in each directory and unified audit logging is on,
  the `unified` stream keeps failing — now with the right message and the
  "needs consent" badge instead of a misleading one.
- A wider retry policy can stretch a throttled call to ~3 minutes; `MaxAttempts:
  3` and the 5-minute `requestTimeout` bound it, so one bad tenant cannot starve
  the rest of the pass.
- The manifest trim is deliberately silent (a rejected name has no events to
  lose); the pinned-name test is the alarm that a curated name was retired.
- Any name added to `driveEventNames` must be checked against Google's event
  reference first: an unverified name fails the whole request, which is exactly
  what caused this incident.
- Message-text changes may break existing assertions in `unified_test.go` and
  `services/auditlog_test.go`; they are updated in the same change.
