# Two report sections that failed on a healthy tenant

## Context

A Microsoft 365 directory report finished `partial` with two unusable sections and
the API's reason attached to each:

```text
team_members  400: Could not find a property named 'email' on type 'microsoft.graph.conversationMember'
planner_plans 403: You do not have the required permissions to access this item
```

Neither was a permission the account lacked in the sense the capability badges
report, and neither was an empty collection:

1. **`team_members`.** The section asked for
   `$select=id,displayName,email,userId,roles`. `email` and `userId` belong to the
   `aadUserConversationMember` *derived* type, while the collection is typed as the
   `conversationMember` *base* type; a tenant whose Graph resolves the projection
   against the base type refuses the whole request. The plain listing — no
   `$select`, the shape the Graph documentation shows — carries `email` as well, so
   the fix is a retry, not a lost column.
2. **`planner_plans`.** The section walked *every* group of the `/groups` page. A
   security group and a distribution list have no Planner workload, so Graph
   answers 403 (or 404); one such group failed the whole table, even after the
   groups that do own plans had answered. The walk was also paying a refused call
   per non-Microsoft-365 group of the directory.

## Goal

- A tenant that resolves the membership projection against the base type still
  reports `team_members`, with its `mail` column, at the price of one extra call
  for the whole section (not one per team).
- `planner_plans` asks only the groups that can own a plan, and a group Graph
  refuses costs that group only.
- A section still fails when the refusal is *total*: an empty `planner_plans` table
  must mean "no plans", never "no grant".
- Both behaviours are pinned by tests and documented in `docs/reports.md`.

## Steps

1. **`teamMembersTarget(teamID string, projected bool)`**
   (`internal/providers/microsoft/report.go`): the `$select` is a parameter, and
   the plain form is the bare collection URL. The doc comment keeps the error text
   and says why the projection is optional.
2. **`walkTeamMembers(teamID, visit)`**: extracted from `teamMembersTable`; reads
   the projected collection, and on `400` with **zero rows collected** reads it
   again plainly while setting `reportData.teamMembersPlain`, so the remaining
   teams are read plainly the first time. The zero-row condition is what keeps a
   mid-listing failure from being masked; rows that were already collected are never
   duplicated, because the retry only runs when there were none.
3. **`reportGroup.isUnified()`**: the `Unified` check `groupKind` already made is now
   a method, used by both the kind column and the Planner walk (one definition of
   "this group can own a plan").
4. **`plannerPlansTable()`**: skips `!group.isUnified()`, and on `403`/`404` from a
   group it records the refusal and continues. It reports the error only when no
   Microsoft 365 group answered and no row was collected — the shape of a missing
   `Tasks.Read` grant — so the "empty means empty" rule survives the degradation.
5. **`isStatus(err, status)`** and **`isGroupRefusal(err)`**: one place that reads a
   Graph refusal, shared by the `loadUsers` licence fallback, the membership retry
   and the plans walk.
6. **Tests** (`internal/providers/microsoft/report_test.go`): the projection
   refusal retried once for the section (and the second team read plainly); the
   plans of a security group never requested; a 403 and a 404 group costing their
   own row only; every group refusing still failing the section; a group with an
   empty plan collection staying a complete section. The "every section" fixture
   drops from 19 to 17 requests, because the distribution list and the security
   group are no longer walked for plans.
7. **Docs**: `docs/reports.md` §1 gains the per-collection degradation paragraph
   (the two bullets), the call table says which groups are walked for plans, and §2
   says a per-group refusal costs that group and not the section.

## Risks

- **One extra call per report** on a tenant that refuses the projection, and one
  refused call per remaining team would have been the alternative — hence the
  remembered refusal on `reportData`.
- **A real 400 on the first page of a team hides behind the retry**: the section
  then fails with the error of the *second* read, which is still a 400 with the
  API's reason, so the log stays truthful.
- **Skipping non-Microsoft-365 groups means plans of a group whose `groupTypes` is
  missing from `$select` are not listed.** The groups walk already selects
  `groupTypes` (the `kind` column needs it), so this only affects a tenant that
  returns an empty `groupTypes`, which cannot own a plan either way.
- **A refused group is invisible in the table.** Partial data is quieter than a
  failed table; the section's `truncated` flag is not set for it, so the reasoning
  is documented rather than shown per row. If an operator needs the list of skipped
  groups, that is a new column, not a revert.
