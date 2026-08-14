# Azure setup

## Authentication

`tellury` uses `DefaultAzureCredential`, so it accepts whatever your environment already
provides — `az login` for a person, a service principal's environment variables for
automation, or a managed identity on an Azure host. It reads no key files and takes no
credentials flag.

```bash
az login                      # interactive
```

```bash
export AZURE_CLIENT_ID=...    # service principal
export AZURE_CLIENT_SECRET=...
export AZURE_TENANT_ID=...
```

## Scopes

Exactly one scope is required. Flags take precedence over environment variables.

| Flag | Environment | Scope |
|---|---|---|
| `--azure-subscription` | `TELLURY_AZURE_SUBSCRIPTION` | One subscription |
| `--azure-resource-group` | `TELLURY_AZURE_RESOURCE_GROUP` | One resource group (requires `--azure-subscription`) |
| `--azure-management-group` | `TELLURY_AZURE_MANAGEMENT_GROUP` | A management group and every subscription under it |
| `--azure-tenant` | `TELLURY_AZURE_TENANT` | The tenant root and every subscription in it |

```bash
tellury scan --azure-subscription 00000000-0000-0000-0000-000000000000
tellury scan --azure-subscription 0000... --azure-resource-group rg-prod-weu-001
tellury scan --azure-management-group mg-engineering
```

`--azure-resource-group` narrows an existing subscription scope rather than standing alone;
it is applied as a filter on the Resource Graph query, so it costs no extra API calls.
Passing it without `--azure-subscription` is a usage error.

A management group or tenant scan fans out **one Resource Graph query per subscription**
rather than handing the whole scope to Resource Graph at once. That is deliberate: it lets a
scan report each subscription's outcome separately, so a subscription you cannot read is
named rather than silently missing from the total.

## Permissions

Everything `tellury` does is read-only. There are two supportable answers, and the trade-off
between them is real.

### The simple answer: Reader

Assign the built-in **Reader** role at the scope you intend to scan. Reader is `*/read`, so
it covers Resource Graph, the resource types the rules read, and the management-group
hierarchy, and it keeps working when new rules are added.

```bash
az role assignment create --assignee <appId> --role Reader \
  --scope "/subscriptions/<subscription-id>"
```

### The least-privilege answer: a custom role

If Reader is too broad, this is every action the scanner calls:

```json
{
  "Name": "Tellury Scanner",
  "Description": "Read-only: Resource Graph plus the resource types tellury's rules read.",
  "Actions": [
    "Microsoft.ResourceGraph/resources/read",
    "Microsoft.Resources/subscriptions/resources/read",
    "Microsoft.Resources/subscriptions/read",
    "Microsoft.Resources/subscriptions/resourceGroups/read",
    "Microsoft.Compute/disks/read",
    "Microsoft.Compute/virtualMachines/read",
    "Microsoft.Compute/virtualMachineScaleSets/read",
    "Microsoft.Compute/skus/read",
    "Microsoft.Compute/galleries/read",
    "Microsoft.Compute/galleries/images/read",
    "Microsoft.Compute/galleries/images/versions/read",
    "Microsoft.Network/publicIPAddresses/read",
    "Microsoft.Insights/metrics/read"
  ],
  "NotActions": [],
  "AssignableScopes": ["/subscriptions/<subscription-id>"]
}
```

| Action | Needed by |
|---|---|
| `ResourceGraph/resources/read` + `Resources/subscriptions/resources/read` | Every rule — all discovery goes through Resource Graph |
| `Resources/subscriptions/read`, `.../resourceGroups/read` | Enumerating scopes, and resolving a resource group scope |
| `Compute/disks/read` | `unattached_managed_disk` |
| `Compute/virtualMachines/read` | `underutilized_vm`, and the reference pass behind `unused_gallery_image_version` |
| `Compute/virtualMachineScaleSets/read` | The same reference pass — see below |
| `Compute/skus/read` | Resource SKUs, to find a smaller VM size for a rightsize recommendation |
| `Compute/galleries/**/read` | `unused_gallery_image_version` |
| `Network/publicIPAddresses/read` | `unassociated_public_ip` |
| `Insights/metrics/read` | Azure Monitor, for `underutilized_vm` |

**On verification:** the live validation runs behind this documentation used the built-in
**Reader** role, so the list above is derived from the API calls in the code rather than
proven by assigning exactly this role and scanning. It was last reconciled against the
codebase on 2026-08-14. If you assign it and a rule reports nothing you expected, check the
role first — and please open an issue, because the paragraph below explains why that failure
is invisible.

```bash
az role definition create --role-definition tellury-role.json
az role assignment create --assignee <appId> --role "Tellury Scanner" \
  --scope "/subscriptions/<subscription-id>"
```

Two things about this role are worth knowing before you choose it.

**`Microsoft.ResourceGraph/resources/read` alone is not enough.** Resource Graph also
requires `Microsoft.Resources/subscriptions/resources/read`. Without it every query returns
`AccessDenied` — which at least fails loudly.

**`virtualMachineScaleSets/read` is safety-critical, not optional.** A gallery image
referenced by a scale set is in use even when no instance is running from it. `tellury`
does not guess: without visible compute it marks the reference inventory untrusted and the
rule skips. Resource Graph omits rows an identity cannot read *without failing*, so the
absence of scale sets is indistinguishable from an identity that cannot see them.

**A missing resource-type action fails SILENTLY, and this is the important one.** Drop
`Microsoft.Compute/disks/read` and the scan still succeeds. It reports `0 resources scanned`
and `No waste found`, because Resource Graph returns an empty result set for resource types
the identity cannot read rather than an error. A permissions gap is therefore
indistinguishable from a clean bill of health.

That is the cost of the custom role: **it must be extended whenever a rule that reads a new
resource type is added**, and forgetting shows up as an empty report rather than a failure.
Reader never needs updating. If nobody owns keeping the role current, prefer Reader — a scan
that quietly finds nothing is worse than one that is slightly over-permissioned.

Whichever you choose, `tellury rules list` shows which rules are registered, and a scan
reporting far fewer resources than you expect is the symptom to check the role against.

### Pricing needs no permissions at all

The Azure Retail Prices API is public and unauthenticated. Unlike GCP and AWS, no role, API
enablement or billing access is needed for `tellury` to price a finding — the identity above
covers discovery only.

## Scanning above a subscription

A management-group or tenant scan needs read access to the hierarchy itself, not only to the
subscriptions under it.

- **Management group scope.** Assign the role at the management group rather than the
  subscription, and it inherits to every subscription beneath:

  ```bash
  az role assignment create --assignee <appId> --role Reader \
    --scope "/providers/Microsoft.Management/managementGroups/<management-group-id>"
  ```

  A custom role used at this scope must list the management group in `AssignableScopes` and
  add `Microsoft.Management/managementGroups/read`.

- **Tenant scope.** `--azure-tenant` walks from the tenant root management group, whose ID
  equals your tenant ID. **Nobody has access to the tenant root by default — not even a
  Global Administrator.** It must be granted once, in the portal:

  Microsoft Entra ID → Properties → *Access management for Azure resources* → **Yes**.

  That grants the signed-in Global Administrator **User Access Administrator at root scope
  (`/`)**, which inherits down the whole tree and is enough on its own to read the hierarchy.
  It is a broad, tenant-wide grant: Microsoft's guidance is to use it to make the specific
  assignments you need and then turn it back off.

  Note that an external identity (a `live.com` or guest account) will not resolve by email in
  `az role assignment create`. Use its object ID:

  ```bash
  az ad signed-in-user show --query id -o tsv
  az role assignment create --assignee-object-id <object-id> \
    --assignee-principal-type User --role Reader \
    --scope "/providers/Microsoft.Management/managementGroups/<tenant-id>"
  ```

**Partial access is reported, not hidden.** In a management-group or tenant scan, a
subscription the identity cannot read is recorded as unreachable with its reason and
reported in the summary; the scan continues and totals the rest. A single-subscription scan
that cannot read its one subscription fails outright instead, because there is nothing left
to report.

## What tellury reads

- **Azure Resource Graph**, one KQL query per subscription, projecting only the columns the
  rules use. Resource Graph returns resource *properties* directly, so there is no per-
  resource follow-up call — unlike the AWS path, where discovery returns identity only.
- **Azure Retail Prices API**, public and unauthenticated, filtered to Consumption rates.

There is no fallback price table. A price that cannot be resolved makes the rule skip the
resource as unpriced rather than reporting a guessed figure.

## Regions

Azure region names reach the graph in their ARM form (`northeurope`), never the display form
(`North Europe`). The two differ as strings, so a scan that mixed them would split one region
into two and misattribute a rollup; canonicalisation happens in exactly one place for that
reason.

A resource group has its own location, and **the resources inside it do not have to share
it** — a group in `westeurope` routinely holds resources in `northeurope`. `tellury` therefore
attributes waste by each resource's own region, and treats the resource group as an attribute
of the resource rather than a location tier.
