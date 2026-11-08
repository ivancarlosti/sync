# Report sections: `analytics` and `youtube`

## Context

The Reports screen renders one tab per section the provider returns, labelled
`reports.section_<name>`. The Google provider declared ten Directory sections and
nothing else, so an operator coming from the n8n/GAM flows found no *Analytics*
and no *YouTube* tab at all — the two collections those flows exported next to
the directory. Nothing was broken: the sections never existed
(`providers.ReportSections` has no `analytics` and no `youtube`, and no locale
carries a label for them).

The requested shape was "Analytics = properties created on analytics.google.com
per user, YouTube = channels created per user". Both APIs stop short of that:

* The **Analytics Admin API** (`analyticsadmin.googleapis.com/v1beta`) lists the
  accounts and properties the *authorised identity* can reach
  (`accounts.list`, `properties.list?filter=parent:accounts/{id}`). A property
  record carries no creator: `createTime` is the only ownership trace it exposes.
* The **YouTube Data API** (`youtube.googleapis.com/youtube/v3`) lists channels
  with `mine=true`, which is the same limit: the channels of the authorised
  identity. Domain-wide channel listings only exist behind a content owner (the
  CMS API), which needs a separate partner identity.

Enumerating *another* user's properties or channels needs domain-wide delegation
(service-account impersonation), which this provider does not implement — it
holds one OAuth grant per connected account (see
[docs/reports.md §2](../docs/reports.md)). The sections therefore report what the
connection reaches and say so, instead of pretending a per-user scope the APIs do
not offer.

## Goal

Two new report sections, collected by the Google provider, selectable like every
other section and degraded like every other section:

| Section | Source | Rows |
|---|---|---|
| `analytics` | `GET analyticsadmin/v1beta/accounts` (paged, 200), then `GET /properties?filter=parent:{account}` per account (paged, 200) | one row per property |
| `youtube` | `GET youtube/v3/channels?part=snippet,statistics&mine=true` (paged, 50) | one row per channel |

Non-goals: per-user attribution (needs DWD), CMS/content-owner data, a new
authentication flow.

## Steps

1. **Vocabulary** (`internal/providers/report.go`): add `ReportSectionAnalytics`
   and `ReportSectionYouTube`, append them to `ReportSections` (new sections are
   appended so a stored report keeps its order) and mark them Google-only in
   `reportSectionExclusive`.
2. **Capabilities** (`internal/providers/provider.go`): add `analytics` and
   `youtube`, append both to `ProviderCapabilities`, mark both Google-only in
   `capabilityExclusive`, so the badge, the guide and *missing permissions* tell
   the truth on an account that has not consented yet.
3. **Permissions** (`internal/providers/google/permissions.go`): declare
   `analytics.readonly` and `youtube.readonly` (both `AdminConsent`, like the
   directory rows: the connection is an administrator identity and the domain's
   API controls allowlist applies), plus the two enablement pages in
   `ConsoleURLs`.
4. **Collector** (`internal/providers/google/report.go`): page sizes
   (`reportAnalyticsPageSize` 200, `reportChannelPageSize` 50), roots, an
   `analyticsAccountPattern` for the value interpolated into the `parent:` filter,
   two record shapes, two targets, two tables and the dispatch entries. A refusal
   fails only its own section — the API enablement of one of the two is
   independent of the other.
5. **Error labels** (`internal/providers/google/client.go`): the two hosts answer
   the same error envelope, and `channels` shares `www.googleapis.com` with Drive,
   so the label is resolved by host **and** path prefix (`youtube api`,
   `analytics admin api`).
6. **Tests**: fixtures for the three endpoints, row/column assertions, the page
   sizes and the filter the property listing sends, an empty reachable catalogue,
   and the per-section refusal.
7. **Docs and i18n**: `docs/reports.md` (section table, calls, the per-user
   boundary), `docs/app-registration.md` (capability table, two more APIs to
   enable), `reports.section_analytic[as]`/`section_youtube` and
   `admin.capability.analytics`/`youtube` in the seven catalogs.

## Risks

* **Consent surface grows.** Two more read-only scopes widen the consent screen
  and the domain allowlist. Existing grants keep working: the sections fail with
  the 403 Google returns and the reason is shown, and *Check the token* offers the
  upgrade.
* **Quota.** `properties.list` is one call per Analytics account, not per
  property, so the cost stays `1 + accounts` calls; `channels.list` is one call
  (50 per page, a person owns a handful).
* **Expectation gap.** "Per user" cannot be answered by these APIs; the docs and
  the section comments say so, so nobody reads the table as an attribution.
* **Analytics account nesting.** `accounts.list` returns only top-level accounts;
  a property that lives under a nested account is still reachable because the
  property listing is asked per account it can see, and the row keeps the account
  display name it was read with.
