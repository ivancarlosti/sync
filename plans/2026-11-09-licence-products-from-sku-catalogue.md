# Licences: derive the catalogue products from the SKU catalogue

## Context

The fix of `2026-11-08-licence-catalogue-primary-domain.md` moved the report from
the per-user fallback to the domain-wide `listForProduct` walk. The users table
then showed Google Workspace Business Standard but stopped showing **Cloud
Identity Free**, which the fallback had reported.

The two walks carry separate product lists:

* `reportLicenseSkus` (the per-user probe) has Cloud Identity Free
  (`101001` / `1010010001`) and Premium (`101005` / `1010050001`).
* `reportLicenseProducts` (the domain-wide listing) was written by hand and only
  contained `Google-Apps`, the Drive/Vault products and several retired product
  ids. It never asked for `101001`.

`listForProduct` only lists the assignments of the product it is asked about, so
Cloud Identity Free was invisible on every tenant whose catalogue answers. It
was only ever seen by the fallback, which is now the exceptional path.

## Goal

* The domain-wide catalogue walks every product the SKU catalogue knows, so a
  licence visible to the per-user fallback is visible to the catalogue as well.
* The product list cannot drift again: it is derived, not duplicated.
* Cloud Identity Free appears next to the paid edition in the `users` licence
  column, and as its own row in the `licenses` section.

## Steps

1. `reportLicenseProducts` becomes `licenseProducts()`, which walks
   `reportLicenseSkus` in order and keeps the unique product ids, then adds the
   legacy `Google-Coordinate` product that has no SKU in the probe catalogue.
2. The existing invalid product ids (`Google-Apps-Unlimited`,
   `Google-Apps-For-Business`, …) are dropped: they are SKUs under
   `Google-Apps`, and the live API answers `400 Invalid productId` for them.
   The probe on `agencia.pub` already showed that.
3. Tests: `TestLicenseProductsCoverTheSkuCatalogue` pins the two lists together;
   `TestLicensesListCloudIdentityFree` scripts a `101001` page and asserts both
   licence cells of the users row and the `licenses` row.
4. `docs/reports.md` says the product list is derived and names the Cloud
   Identity SKUs.

## Cost

The walk grows from 11 to 20 product calls. A product without assignments
answers an empty page, and the retired ids are gone, so this is still a handful
of cheap calls compared with the 77-per-user fallback it replaces.

## Risks

* **A new SKU with a new product id** is picked up automatically by both walks.
* **A product Google rejects with 400** is skipped exactly as before.
* **The legacy Coordinate product** stays in the list because there is no SKU
  entry to derive it from; it answers an empty page on a tenant that does not
  own it.
