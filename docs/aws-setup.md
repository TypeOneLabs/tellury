# AWS setup for tellury

tellury scans AWS accounts for waste: unattached EBS volumes, unassociated Elastic IPs,
overprovisioned EC2 instances, unused AMIs and the EBS snapshots a deregistered AMI leaves
behind.

## Required permissions

The permissions a scan needs depend on the **scope flag**, on **how regions are
chosen**, and on **which account each API call runs against**.

| Scope | Caller account needs | Every member account needs (through the assumed role) |
|---|---|---|
| `--aws-account <id>` | The **per-account read permissions** below, plus `pricing:GetProducts`. The account is scanned directly; no role is assumed. | None. |
| `--aws-organizational-unit <ou>` | The **per-account read permissions** below (for the caller's own account), plus `pricing:GetProducts`, the **organization permissions** below, and `sts:AssumeRole`. The caller's own account is scanned directly and **needs no role**. | The **per-account read permissions** below, granted to the assumed role. These permissions are checked in **each member account**, not in the caller's account. |
| `--aws-organization <o-...>` | Same as `--aws-organizational-unit`. | Same as `--aws-organizational-unit`. |

The caller's own account is scanned directly and needs no role. Every other
account in scope is scanned by assuming that account's role, so the per-account
read permissions must exist **in each member account**. If you grant the read
permissions only to the caller, the member-account scans fail with
`AccessDenied` (or the accounts are reported unreachable), and the summary says
the scan was degraded without making the missing member-account grant obvious.

### Per-account read permissions

These are the actions tellury uses against **the account being scanned**. In a
single-account scan they must exist in that account. In an organization or OU
scan they must exist in the caller's own account AND in every member account,
granted through the assumed role.

| Action | Purpose |
|---|---|
| `ec2:DescribeRegions` | Enumerate the account's enabled regions. Required by the **default** scan, and by the Resource Explorer per-region sweep when `--aws-use-resource-explorer` is set without an aggregator index. |
| `ec2:DescribeVolumes` | List every EBS volume in a region (paginated). |
| `ec2:DescribeAddresses` | List every Elastic IP in a region. |
| `ec2:DescribeInstances` | List every EC2 instance in a region. Needed by any instance rule. |
| `ec2:DescribeInstanceTypes` | Read each instance type's vCPU and memory. Used to judge whether a smaller size would fit; tellury reads these live rather than shipping a table that goes stale. |
| `ec2:DescribeImages` | List customer-owned AMIs. Needed by `unused_ami`, and by `orphaned_ami_snapshot` to know which snapshots a live AMI still backs. |
| `ec2:DescribeSnapshots` | List EBS snapshots — both to price an AMI's backing storage for `unused_ami` and to find the snapshots `orphaned_ami_snapshot` reports. |
| `ec2:DescribeLaunchTemplates`, `ec2:DescribeLaunchTemplateVersions` | Find AMIs referenced by a launch template. **An AMI referenced by a template with no running instances is still in use**, so without these an in-use AMI can be reported as unused. |
| `ec2:DescribeFleets`, `ec2:DescribeSpotFleetRequests` | The same, for AMIs referenced inline by EC2 Fleet overrides and Spot Fleet launch specifications. |
| `autoscaling:DescribeLaunchConfigurations` | The same, for Auto Scaling launch configurations. Note this is an `autoscaling:` action, not `ec2:`. |
| `cloudwatch:GetMetricData` | Read instance CPU utilization. Without it, metric-dependent rules skip and say so. Note this call sits outside the CloudWatch free tier at $0.01 per 1,000 metrics requested — a scan needs tens of thousands of instances before that rounds to a cent. |
| `resource-explorer-2:Search` | **Only with `--aws-use-resource-explorer`.** Find which regions hold the resource types the selected rules need. Note this call CREATES an index in an un-indexed region — a scan with this flag performs writes. |
| `resource-explorer-2:ListIndexes` | **Only with `--aws-use-resource-explorer`.** Learn which regions Resource Explorer can already answer for, so the ones it cannot are reported rather than silently empty. |

The default scan does **not** need `resource-explorer-2:Search` or
`resource-explorer-2:ListIndexes`. It uses only `ec2:DescribeRegions` and the
per-region service APIs, and performs no writes.

### Caller-only permissions

These permissions are checked against the **caller's own IAM principal**. They
do not need to be added to member-account roles.

| Action | Needed for | Purpose |
|---|---|---|
| `pricing:GetProducts` | Every scope | Load the live Price List catalogue. Without it every resource that needs a price is skipped as unpriced — there is no fallback table. Pricing is always called with the caller's credentials, never with a member-account role. |
| `organizations:DescribeOrganization` | `--aws-organization`, `--aws-organizational-unit` | Read the organization ID (the `o-` string). |
| `organizations:ListRoots` | `--aws-organization`, `--aws-organizational-unit` | List the root(s) of the organization. |
| `organizations:ListOrganizationalUnitsForParent` | `--aws-organization`, `--aws-organizational-unit` | Walk the OU hierarchy. |
| `organizations:ListAccountsForParent` | `--aws-organization`, `--aws-organizational-unit` | List every account under a root or OU. |
| `sts:AssumeRole` | `--aws-organization`, `--aws-organizational-unit` | Assume the cross-account role in each member account. Called once per account. |

### The cross-account role

Every member account (except the caller's own — the management account or a
delegated administrator that holds the credentials tellury runs under) needs an
IAM role that the caller can assume. By convention AWS Organizations creates
this role as **`OrganizationAccountAccessRole`** when an account is created
through the console.

The role must grant at minimum the **per-account read permissions** above. It
does not need `pricing:GetProducts` or the organization permissions.

If your organization uses a different role name, pass it with:

```
--aws-role-name MyCustomRoleName
```

The default is `OrganizationAccountAccessRole`.

## Scanning an organization or OU

```bash
tellury scan --aws-organization o-xxxxxxxxxx
tellury scan --aws-organizational-unit ou-xxxx-xxxxxxxx
```

The organization must be the one your credentials belong to. `DescribeOrganization` answers
for the caller and takes no ID, so a mismatched `--aws-organization` is refused rather than
silently traversing a different organization under the requested name.

Accounts are reached by assuming a role, `OrganizationAccountAccessRole` by default. Two
things follow:

- **Your own account needs no role.** The account your credentials belong to — usually the
  management account — is scanned directly. You cannot assume into yourself unless someone
  has explicitly created a role, and Organizations creates that role in accounts it creates,
  not in the management account.
- **Every other account needs that role to exist there**, and your identity must hold
  `sts:AssumeRole` on it. Accounts that refuse are reported by name and reason in both the
  summary and the JSON, rather than dropped — a total that quietly omits part of an
  organization is worse than an error.

```
SUMMARY
--------------------------------------------------------------------------
Scope          o-xxxxxxxxxx
Scope ID       organizations/o-xxxxxxxxxx
Status         ok
Scanned        5 resources (4 skipped) across 2 accounts
...

COVERAGE
Account outcomes: 1 scanned, 1 unreachable (AccessDenied assuming OrganizationAccountAccessRole)
  scanned:     171140037492 (management) — 17 of 17 regions searchable
  unreachable: 222222222222 (prod) — cannot assume role OrganizationAccountAccessRole
```

Each account also reports how many of its enabled regions were actually searchable, in the
table and in the JSON (`regions_searchable` / `regions_enabled`). Under the default mode this
is always every enabled region. Under `--aws-use-resource-explorer` it is the number of
Regions Resource Explorer could answer for, which is how a brand-new account is told apart
from a genuinely clean one.

`Status` is `degraded` whenever an account could not be reached, so a partial scan is
visible at a glance and in the JSON as `scan_status`, not only in the outcomes list.

## Choosing regions: default sweep, explicit regions, or Resource Explorer

There are three region modes, in precedence order:

1. **`--aws-regions <list>`** — scan exactly those regions. Resource Explorer is never
   called. This is the fastest mode and the recommended way to speed up the default.
2. **`--aws-use-resource-explorer`** — narrow regions through AWS Resource Explorer: an
   aggregator query if an aggregator index exists, otherwise the per-region sweep. Fast, may
   be incomplete on early runs, and creates indexes.
3. **neither (the default)** — enumerate enabled regions with `ec2:DescribeRegions` and
   hydrate all of them through the service APIs. Slower (about 70s versus 15s on a
   17-region account), 100% complete on the first run, and makes **no** Resource Explorer
   calls. A default scan performs no writes.

The default is the right choice for a first look at an account. It reads every enabled
region directly, so an empty result means the account is actually empty (or the identity
cannot read the resource types), not that a Resource Explorer index has not been created
yet.

```bash
# First look: complete, read-only, no Resource Explorer calls.
tellury scan --aws-account 123456789012

# Faster follow-ups: name the regions you know you use.
tellury scan --aws-account 123456789012 --aws-regions us-east-1,us-west-2
```

### `--aws-use-resource-explorer` is opt-in

Resource Explorer only knows about a Region that has an **index**. AWS is explicit:
*"If you do not create a user-owned index in a Region, resources from that Region will not
appear in cross-region search results from other Regions."* An un-indexed Region does not
return an error when searched — it returns:

```json
"Count": { "TotalResources": 0, "Complete": true }
```

A confident empty, indistinguishable from a Region that is genuinely idle.

Searching an un-indexed Region **creates** its index as a side effect. So with
`--aws-use-resource-explorer`, the first scan of a Region may report nothing there, and a
later scan sees it once the index populates — minutes for tagged resources, up to about two
hours for untagged ones. Consequences:

- **The first scans of a new account under-report, and coverage builds up over several
  runs.** Index creation is asynchronous and only a few Regions are indexed per scan.
  Measured on a fresh account: 3, then 8, then 12, then 14 of 17 Regions searchable across
  four scans. Do not assume one repeat run is enough — read the per-account coverage line
  (below) and treat the total as complete only once it stops moving. This is fine for a
  regular scheduled CI check; it is the wrong trade for a first manual look at an account.
- **A newly indexed Region stays empty for a while.** Its index exists, so it is no longer
  listed as missing, but it has not finished populating.
- **The flag writes.** Index creation is the one non-read-only thing a scan can do, and it
  happens only on this path. The default scan and `--aws-regions` never create an index.

When using the flag, tellury names the Regions it knows Resource Explorer cannot yet answer
for:

```
WARN regions have no Resource Explorer index yet; resources there are NOT
     reported by this scan ... regions=11 list=ap-northeast-1,ap-south-1,...
```

**To scan a Region immediately, name it.** Explicit regions skip Resource Explorer entirely
and read the EC2 APIs directly, so nothing is missed and nothing is created:

```bash
tellury scan --aws-account 123456789012 --aws-regions us-west-1
```

An empty result from a Region that IS indexed is a real answer: nothing of those types is
there.

```
SUMMARY
--------------------------------------------------------------------------
Scope          o-xxxxxxxxxx
Scanned        41 resources across 3 accounts
Regions        4 regions (resource_explorer)
...
```

The region-source annotation appears after the region count in both the summary block and
the JSON output (`region_source` field). It is one of:

- `explicit` — `--aws-regions` named the regions.
- `describe_regions` — the default `ec2:DescribeRegions` sweep.
- `resource_explorer` — `--aws-use-resource-explorer` narrowed the regions.
- `fixture` — an offline fixture replay named the regions.

### Staleness

The Resource Explorer index is eventually consistent. Tagged resources appear within
minutes; untagged resources can take up to two hours. Because hydration always reads the
live EC2 APIs — never the index's cached attributes — a stale index can only cause a very
recently created resource to be missed. It can never produce a stale attribute or a wrong
price.

## Example minimal IAM policies

Policy for the **caller** on a single-account scan using the **default region mode**
(`--aws-account`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeRegions",
        "ec2:DescribeVolumes",
        "ec2:DescribeAddresses",
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeImages",
        "ec2:DescribeSnapshots",
        "ec2:DescribeLaunchTemplates",
        "ec2:DescribeLaunchTemplateVersions",
        "ec2:DescribeFleets",
        "ec2:DescribeSpotFleetRequests",
        "autoscaling:DescribeLaunchConfigurations",
        "cloudwatch:GetMetricData",
        "pricing:GetProducts"
      ],
      "Resource": "*"
    }
  ]
}
```

If you opt into `--aws-use-resource-explorer`, add these two actions to the per-account
policy (and to each member-account role for organization scans):

```json
"resource-explorer-2:Search",
"resource-explorer-2:ListIndexes"
```

For organization scans, add these caller-only permissions to the caller policy:

```json
{
  "Effect": "Allow",
  "Action": [
    "organizations:DescribeOrganization",
    "organizations:ListRoots",
    "organizations:ListOrganizationalUnitsForParent",
    "organizations:ListAccountsForParent",
    "sts:AssumeRole"
  ],
  "Resource": "*"
}
```

The `sts:AssumeRole` resource should be scoped to the role ARN pattern in
member accounts:

```json
{
  "Effect": "Allow",
  "Action": "sts:AssumeRole",
  "Resource": "arn:aws:iam::*:role/OrganizationAccountAccessRole"
}
```

And attach this policy to the **role in each member account** (or otherwise
grant the same actions to the role) for a default or `--aws-regions` scan:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeRegions",
        "ec2:DescribeVolumes",
        "ec2:DescribeAddresses",
        "ec2:DescribeInstances",
        "ec2:DescribeInstanceTypes",
        "ec2:DescribeImages",
        "ec2:DescribeSnapshots",
        "ec2:DescribeLaunchTemplates",
        "ec2:DescribeLaunchTemplateVersions",
        "ec2:DescribeFleets",
        "ec2:DescribeSpotFleetRequests",
        "autoscaling:DescribeLaunchConfigurations",
        "cloudwatch:GetMetricData"
      ],
      "Resource": "*"
    }
  ]
}
```

Add `resource-explorer-2:Search` and `resource-explorer-2:ListIndexes` to the
member-account role only if that role will be used with
`--aws-use-resource-explorer`.

Notice that `pricing:GetProducts` and the organization permissions are
**caller-only** and are absent from the member-account role policy.

## Credentials

tellury reads no key files. It uses the standard AWS SDK credential chain:
environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_SESSION_TOKEN`), the shared credentials file (`~/.aws/credentials`), and
the profile named by `AWS_PROFILE`. Run `aws configure` or set the environment
variables — exactly as you would for the AWS CLI.
