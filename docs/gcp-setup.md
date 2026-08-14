# GCP setup

## Authentication

Application Default Credentials only. `tellury` reads no key files and takes no credentials
flag.

```bash
gcloud auth application-default login
```

Service-account impersonation works through ADC in the usual way.

## APIs

Enable `cloudasset`, `monitoring` and `cloudbilling` on the project the credentials belong
to.

## Permissions

Everything `tellury` does is read-only. Each role unlocks one capability; only the first is
required. Without any of the others a scan still runs and reports what it could not do.

| Role | Granted on | Enables | Without it |
|---|---|---|---|
| `roles/cloudasset.viewer` | scanned org, folder or project | Resource discovery | Nothing to scan; required |
| `roles/monitoring.viewer` | each project in scope | Metric enrichment | Metric-dependent rules skip, each saying why |
| `roles/compute.viewer` | each project in scope | Instance-template references, for `unused_custom_image` | That rule skips every image as `references_unknown` |
| `roles/browser` | the org, folder or project | Resolving a project to its billing account | Currency detection stops at step one; figures are USD |
| `roles/billing.viewer` | the billing account | Reading that account's currency | Figures are USD |

**Live pricing needs no billing role.** The Cloud Billing Catalog is readable by any
authenticated caller. The last two rows buy only the ability to report figures in the
currency you are invoiced in.

```bash
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/cloudasset.viewer"
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/monitoring.viewer"

# Image rules: instance-template references (see below).
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/compute.viewer"

# Currency detection: hierarchy read, then the billing account's currency.
gcloud organizations add-iam-policy-binding ORG_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/browser"
gcloud billing accounts add-iam-policy-binding BILLING_ACCOUNT_ID \
  --member="serviceAccount:SA_EMAIL" --role="roles/billing.viewer"
```

### Why the image rule needs more than Cloud Asset Inventory

`unused_custom_image` reports an image nothing references. Cloud Asset Inventory lists the
images themselves, but it does not tell you what points *at* them — so the rule pages through
`compute.instanceTemplates.list` in each project to collect
`properties.disks[].initializeParams.sourceImage`.

That call is not covered by `roles/cloudasset.viewer`. The single permission it needs is
`compute.instanceTemplates.list`; `roles/compute.viewer` is the convenient built-in that
contains it, and a custom role with just that permission works equally well.

Without it the rule does not guess. It records `references_unknown` for every image and
reports nothing, because **an image referenced by an instance template with no running
instances is still in use** — a template that launches later will fail if the image is gone.
The client is built lazily, only when a scan actually ingests images, so scans that run no
image rule never need this grant.

### Impersonation

If you authenticate by impersonating a service account, the **caller** also needs
`roles/iam.serviceAccountTokenCreator` on that service account:

```bash
gcloud iam service-accounts add-iam-policy-binding SA_EMAIL \
  --member="user:YOU@example.com" --role="roles/iam.serviceAccountTokenCreator"
```

This catches people out: the service account can hold every role above and impersonation
still fails with a 403 on `iam.serviceAccounts.getAccessToken`, because that permission
governs who may borrow the identity rather than what the identity may do.

## Currency

By default a scan prices in USD. `--currency EUR` (or `TELLURY_CURRENCY=EUR`) prices the
whole scan in that currency — the Cloud Billing Catalog converts the catalogue server-side,
so figures are expressed in EUR rather than converted afterwards by the tool.

With no flag, tellury detects the currency from the billing account of a project in scope.
A billing account's currency is fixed at creation, so it is a reliable answer. Detection
takes two hops needing different grants — `roles/browser` resolves the project to its
billing account, `roles/billing.viewer` reads that account's currency — and any failure
degrades quietly to USD.

Every report states which currency the figures are in and how it was decided:

```
Prices are in EUR (detected from the billing account).
```

Two things worth knowing:

- **There is no fallback price table.** If the live catalogue cannot answer, the resource is
  skipped as unpriced rather than given a guessed figure — so a scan without billing access
  reports what it found and could not price, never a number it invented.
- **GCP reports a project you cannot see and a project that does not exist identically**,
  as `PermissionDenied`. A fallback to USD is not by itself proof that a role is missing —
  check the project name too.

## What tellury reads

- **Cloud Asset Inventory**, through `SearchAllResources` rather than `ListAssets`.
  `ListAssets` is fronted by a snapshot that can lag by hours and omit resources entirely;
  a reserved address can still report `RESERVING` long after it is `RESERVED`. A waste
  figure is only honest against the scope as it is now.
- **Cloud Monitoring**, fetched concurrently with bounded parallelism. Percentiles are
  computed client-side so the aggregation is reproducible.
- **Cloud Billing Catalog**, cached for the scan.

Without billing access, resources that need a price are skipped as unpriced. Without
monitoring access, metric-dependent rules skip and say why. Neither fails the scan, and both
say plainly what they could not do.
