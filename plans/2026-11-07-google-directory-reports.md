# Google Workspace directory reports

## Context

The n8n era had two collectors: "Microsoft 365 using MS Graph" and "Google
Workspace using GAM". The first is a first-class screen today
(`internal/providers/microsoft/report.go`, [reports.md](../docs/reports.md)); the
second still needs a GAM binary, a service account with domain-wide delegation
and an SSH session into the box that runs it.

Everything GAM reads for that workflow is already reachable over the OAuth
connection a Workspace account has: the Admin SDK Directory API, the Enterprise
License Manager API and the Drive API are the same grant the capability badges of
the Accounts screen are derived from (`internal/providers/google/permissions.go`).
The collector was missing, not the permission.

Two constraints shaped the design:

* the section vocabulary was Microsoft specific (`guests`, `distribution_lists`,
  `sites`), so it had to become provider aware before a second collector could
  exist;
* `GET /customers/my_customer` — the tenant record behind the `organization`
  section — is the one call no requested scope covered.

## Goal

A Google Workspace account produces a directory report with the same lifecycle as
the Microsoft one (async run, per-section tables, CSV export, schedules, pruning)
covering: `organization`, `users`, `suspended_users`, `groups`, `group_members`,
`domains`, `org_units`, `roles`, `role_assignments`, `licenses` and
`shared_drives`.

The user asked for the most complete version, accepting a new scope and an
account reconnect, so `admin.directory.customer.readonly` was added to the
permission table instead of deriving the `organization` row from the domain
list. The fallback is kept anyway: an account connected before the row existed
answers 403 there and gets a derived row (the `source` column says `domains`)
instead of a failed section.

## Steps

1. **Vocabulary** (`internal/providers/report.go`): append six sections
   (`suspended_users`, `group_members`, `org_units`, `roles`, `role_assignments`,
   `shared_drives`) to `ReportSections` — which stays the validation union the API
   accepts — and add `reportSectionExclusive` plus `ReportSectionsFor(name)`, the
   per-provider catalogue. Appending (not inserting) keeps the table order of an
   already collected report.
2. **Microsoft** (`internal/providers/microsoft/report.go`): `ReportSections()`
   answers `providers.ReportSectionsFor(models.ProviderMicrosoft)`; its test is
   updated to compare against that half.
3. **Google collector** (`internal/providers/google/report.go`): a
   `providers.ReportSource` mirroring the Microsoft one — `reportData` cache,
   per-section `Error`/`Truncated`, `ReportProgress` — with Google's own paging
   (`nextPageToken`), its own bounds (20 000 rows, 500 pages per walk) and its own
   unions: the users page feeds `users` and `suspended_users`, the groups page
   feeds `groups` and the per-group walk of `group_members`, and the licence
   catalogue feeds both the `licenses` section and the `licenses` column of
   `users`.
4. **Permission** (`internal/providers/google/permissions.go`): add
   `admin.directory.customer.readonly` under the `domains` capability, with the
   console/guide/i18n consequences that follow from the "one always-on scope set"
   rule.
5. **Guards**: the page token (`pageTokenPattern`), the group key
   (`groupKeyPattern`) and the licence product id (`licenseProductPattern`) are
   narrowed inline where they are interpolated, and each is escaped (`url.PathEscape`,
   `withParam`) before it reaches a URL — the same two layer rule the Drive
   identifiers already follow.
6. **Tolerance**: a product id the API retired answers 400 and is skipped; a group
   the directory refuses to enumerate is skipped; a refused licence catalogue
   empties one column instead of losing the users table; a refused customer record
   degrades to the derived row. A section only fails when *nothing* it needs
   answered.
7. **i18n**: `reports.section_*` for the six new sections in all seven catalogs.
8. **Docs**: [reports.md](../docs/reports.md) §1, §2, §3 and §6,
   [architecture.md](../docs/architecture.md) §3c,
   [api.md](../docs/api.md) §8.2 and
   [app-registration.md](../docs/app-registration.md) §1.
9. **Tests** (`internal/providers/google/report_test.go`,
   `internal/providers/report_test.go`): a full pass over a scripted Workspace
   domain (row counts per section, derived sections, licence column from the
   License Manager walk, call counts), the customer-scope fallback, per-section
   isolation, the page-token guard, the retired product, the refused catalogue,
   and the provider affinity of the vocabulary.

## Risks

| Risk | Mitigation |
|---|---|
| A new scope forces a reconnect | The row is additive: the guide explains it, `needs_reconnect` reports it, and the `organization` section still works (derived row) until the operator upgrades |
| `group_members` costs one call per group | It is opt-in per report, bounded by the shared 20 000 row budget, and documented as the one section that scales with the number of groups |
| The licence catalogue is a hardcoded product list | Same list GAM carries, a retired id answers 400 and is skipped, and a product with no assignment answers an empty page |
| Tenants where an administrator is not a super admin | The Directory API answers 403 on the role endpoints (`roles`, `role_assignments`) and the section is reported as failed while the rest of the report survives — which is the existing per-section contract |
| A page token could steer the next request | The token is validated against an alphabet before it is escaped and appended; a token that is not a token fails the section |
| The vocabulary could drift from the collectors | `report_test.go` asserts every section is offered to at least one provider, that the exclusive sections landed on the right one, and that a shared section is in both lists |
