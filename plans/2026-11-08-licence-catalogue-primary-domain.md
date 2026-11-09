# Licences: scope the catalogue to the primary domain, not only the customer id

## Context

`report-23` of the resold tenant `agencia.pub` finished `success` in 191 s with
`truncated=true`, 23 users and 19 users left unread. The log line named the cause:

```
the per-user licence lookup left users unread users=19 probed=23
reason="google licensing api: http 403 (rateLimitExceeded): Quota exceeded for
quota metric 'Queries' and limit 'Queries per minute per user' of service
'licensing.googleapis.com' for consumer 'project_number:756446029808'."
```

The domain-wide catalogue had been refused, so `loadLicenses` fell back to
`probeLicenses`: one batch of the 77-SKU catalogue per user, 77 licensing queries
per user. Google's `Queries per minute per user` quota is exhausted after a
handful of users, and the 2 s/4 s retries never reach the next minute window.

The 403 was not about the subscription. Live probes with the connected account
(`ivan.almeida@agencia.pub`) show:

| request | result |
|---|---|
| `GET /customers/my_customer` | `id=C045l8wpy`, `customerDomain=agencia.pub` |
| `listForProduct Google-Apps customerId=C045l8wpy` | `403 Unauthorized operation for the given domain` |
| `listForProduct Google-Apps customerId=my_customer` | `403 Unauthorized operation for the given domain` |
| `listForProduct Google-Apps customerId=agencia.pub` | `200`, 19 assignments |

The License Manager API guide documents `customerId` as the customer's primary
domain; the code used the Admin-console id. The fallback was a workaround for a
wrong scope, and the workaround was what exhausted the quota.

## Goal

* The catalogue is tried with the primary domain first, then the Admin-console id,
  then the `my_customer` alias. A 403 refuses the *value*, not the product, so the
  product walk stops and the next value is tried.
* Only when every value is refused does the per-user fallback run. The fallback
  stays for a domain that genuinely cannot list the catalogue.
* A user still gets the licence from the catalogue in one walk of 11 product
  calls, so the `Queries per minute per user` quota is not touched.

## Evidence after the fix

`cmd/reportprobe` with the same collector against `agencia.pub`:

```
collected in 12.23s
users              23 rows
licenses           19 rows
licence cells of the users section (23 users):
  19 hold at least one licence
  4 the API answered with none (license count 0)
  0 left blank: the lookup never answered
```

## Steps

1. `licenseCustomers` (`internal/providers/google/report.go`) returns the values
   in order: `customerDomain`, `customerId`, `my_customer`, deduplicated.
2. `loadLicenses` tries each value; a 403 stops the product walk for that value
   and moves to the next; the first value that answers a product wins.
3. `scanLicenses` keeps taking the resolved value; comments and `docs/reports.md`
   say why the order is domain first.
4. Tests: the live tenant (domain answers, id refused) uses the domain and never
   opens a batch; the inverse (id answers, domain refused) falls back to the id;
   the existing all-refused test now expects one refusal per value before the
   per-user fallback.

## Risks

* **One extra call** when a value is refused. That is cheaper than the 77-query
  batches it prevents.
* **Domain aliases.** The customer record's `customerDomain` is the primary domain
  the guide documents; a tenant whose catalogue only answers its id still works
  because the id is the second candidate.
* **The fallback is unchanged.** A tenant that refuses every value can still hit
  the licensing quota; the live tenant no longer reaches that path, and the
  existing truncation/logging keeps the unread users visible instead of reporting
  them as zero.
