# Report sections: the `rows` column closed a whole report

## Context

Reports of a large Microsoft tenant finished as `failed` with nothing in them, and
the container log carried the reason at the very moment of the write:

```text
Error 1406 (22001): Data too long for column 'rows' at row 1
```

Three separate weaknesses met in that one line:

1. **The column was too small.** `report_sections.rows` was declared `TEXT`, which
   stops at 65 535 bytes on MySQL/MariaDB. A section is not a header, it is a whole
   table of the directory — `team_members` and `planner_plans` are the first ones
   to grow, because they hold one row per membership — and their JSON document is
   measured in megabytes on a tenant with thousands of users and teams.
2. **One INSERT carried the whole pass.** `Store.AddReportSections` wrote every
   table of the report with a single `CreateInBatches`. The database rejects a
   statement as a whole, so the one oversized document discarded the tables of the
   sections that were perfectly fine, and the report was closed empty.
3. **Nothing bounded the document.** The collectors cap a section at
   `reportRowLimit` = 20 000 rows, a budget of *rows*, not of bytes: the same
   20 000 rows are a few hundred kilobytes of domains and several megabytes of team
   memberships, and the second kind had no ceiling below the column's.

## Goal

- A large report stores every section it collected, on the databases that already
  exist (the fix must reach an instance that has been running, not only a new
  install).
- No single oversized section can empty a report: a section the database refuses
  costs that section only, and is reported like one the provider could not fill.
- One document never outgrows the packet a single statement may carry, so the
  failure cannot come back as `Packet too large` with the column widened.
- The documentation says which type the column is, why, and where the budget is.

## Steps

1. **Model** (`internal/models/report.go`): `ReportSectionData.Rows` becomes
   `gorm:"type:longtext"` (4 GiB) instead of `type:text`; `Columns` stays `TEXT`
   (a header is a handful of key/title pairs). The comment keeps the error message
   and the reasoning, so the next reader does not shrink it back.
2. **Migration**: no raw `ALTER` is added. `database.Migrate` runs `AutoMigrate` on
   every boot and GORM alters a column whose declared type is not the stored one
   (`text` is not a prefix of `longtext`, and the only type aliases the MySQL
   driver declares are `bool`/`tinyint`), so `TEXT` → `LONGTEXT` is a
   `MODIFY COLUMN` on the first boot after the upgrade. Pinned by
   `TestMigrateWidensTheReportSectionRowsColumn`, which migrates a database built
   with the old schema and reads the DDL back.
3. **One INSERT per section** (`internal/services/store.go`,
   `internal/services/report.go`): `AddReportSections` is replaced by
   `AddReportSection`, and `storeTables` now answers a `storedTables` summary
   (sections stored, rows stored, truncation, the names the database refused)
   instead of an error for the whole pass. A refused section is logged with its
   error, the row count and the size of the document, and lands in
   `failed_sections` so the run closes as `partial` and the message names it. A
   pass where *no* section could be stored is a failure (`errNoSectionStored`).
4. **Document budget** (`reportSectionBytes` = 8 MiB): `buildReportSection` encodes
   the rows and, when the document is over the budget, keeps the longest prefix
   that fits (binary search over the rows — a few encodings, not one per row) and
   flags the section `truncated`, exactly like a section that hits the row cap. The
   counters of the run describe what was stored, not what was walked.
5. **Tests** (`internal/services/report_test.go`, `internal/database/database_test.go`):
   the DDL GORM emits for MySQL declares `longtext` for `rows`; a 4 000-row
   `team_members` document (well past 64 KiB) round-trips; the sections around a
   refused one survive; `buildReportSection` cuts at the budget and keeps the flags
   of the collector; an existing `TEXT` column is widened by `Migrate` with its
   rows intact and a second `Migrate` is a no-op.
6. **Docs**: `docs/database.md` (the column is `LONGTEXT`, the reason, the 8 MiB
   bound, and that a widened column is altered on the first boot of an upgrade) and
   `docs/reports.md` §3 (the byte budget beside the row budget, and a section the
   database refuses being reported like one the provider refused).

## Risks

- **The `MODIFY COLUMN` copies the table once** on the first boot after the
  upgrade, and `report_sections` holds up to 20 reports per account. That is a
  one-off cost of seconds on a healthy instance; a large deployment should take the
  upgrade during a quiet window (documented in `docs/database.md` §5).
- **A section cut at 8 MiB is incomplete**, like one cut at 20 000 rows: the flag
  `truncated` says so and the UI already renders it. The alternative — widening
  until the statement fails — loses the section entirely, which is worse.
- **`max_allowed_packet` is a server setting.** 8 MiB fits the stock 16 MiB of
  MariaDB and the 64 MiB of MySQL 8, but a server configured below 9 MiB would
  still refuse the statement; the section is then reported as failed instead of
  taking the report down, which is the improvement this change makes.
- **N INSERTs instead of one** is more round trips per pass (18 sections at most),
  against the risk of losing 17 tables to the 18th.
- `Store.AddReportSections` disappears; every caller (`services/report.go`, the two
  test seeds) is updated in the same change, so no partial rename is left behind.
