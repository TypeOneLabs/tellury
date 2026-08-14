# AWS setup for tellury

tellury scans AWS accounts for waste — unattached EBS volumes and unassociated
Elastic IPs today, with more resource types to follow.

## Required permissions

Every permission below is an IAM action the caller's credentials must allow.
The table groups them by whether they are needed for every scan or only for
organization-level scans.

### Every AWS scan

| Action | Purpose |
|---|---|
| `ec2:DescribeRegions` | Discover which regions the account has enabled. |
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
| `pricing:GetProducts` | Load the live Price List catalogue. Without it every resource that needs a price is skipped as unpriced — there is no fallback table. |
| `resource-explorer-2:Search` | Find which regions hold the resource types the selected rules need. Needed unless you pass `--aws-regions`: there is no EC2 `DescribeRegions` sweep. Note this call CREATES an index in an un-indexed region — the one write a scan performs. |
| `resource-explorer-2:ListIndexes` | Learn which regions Resource Explorer can already answer for, so the ones it cannot are reported rather than silently empty. |

### Organization / OU scans only

These permissions are needed ONLY when scanning with `--aws-organization` or
`--aws-organizational-unit`. A single-account scan (`--aws-account`) does not
use them.

| Action | Purpose |
|---|---|
| `organizations:DescribeOrganization` | Read the organization ID (the `o-` string). |
| `organizations:ListRoots` | List the root(s) of the organization. |
| `organizations:ListOrganizationalUnitsForParent` | Walk the OU hierarchy. |
| `organizations:ListAccountsForParent` | List every account under a root or OU. |
| `sts:AssumeRole` | Assume the cross-account role in each member account. Called once per account. |

### The cross-account role

Every member account (except the caller's own — the management account or a
delegated administrator that holds the credentials tellury runs under) needs an
IAM role that the caller can assume. By convention AWS Organizations creates
this role as **`OrganizationAccountAccessRole`** when an account is created
through the console.

The role must grant at minimum the **Every AWS scan** permissions above.

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
```

`Status` is `degraded` whenever an account could not be reached, so a partial scan is
visible at a glance and in the JSON as `scan_status`, not only in the outcomes list.

## Resource Explorer decides which regions are scanned

Resource Explorer is how tellury knows which regions to look in. There is no
`DescribeRegions` sweep of the EC2 APIs: sweeping every enabled region took ~70s
against an account whose resources sat in two. The same account now scans in
~15s.

**You do not have to set anything up.** If the account has an aggregator index,
one query per resource type covers every indexed region. If it does not — which
is most accounts — tellury asks each enabled region directly. Either way the
scan then hydrates only the regions that came back with something.

### Data can be missing on the first runs

This is the cost of requiring no setup, and it is worth understanding before you
trust a first scan.

Resource Explorer only knows about a Region that has an **index**. AWS is
explicit: *"If you do not create a user-owned index in a Region, resources from
that Region will not appear in cross-region search results from other Regions."*
An un-indexed Region does not return an error when searched — it returns:

```json
"Count": { "TotalResources": 0, "Complete": true }
```

A confident empty, indistinguishable from a Region that is genuinely idle.

Searching an un-indexed Region **creates** its index as a side effect. So the
first scan of a Region reports nothing there, and a later scan sees it once the
index populates — minutes for tagged resources, up to about two hours for
untagged ones. Two consequences:

- **The first scan of a new account under-reports.** Expect to run it twice, a
  couple of hours apart, before treating the total as complete.
- **A newly indexed Region stays empty for a while.** Its index exists, so it is
  no longer listed as missing, but it has not finished populating.

tellury names the Regions it knows Resource Explorer cannot yet answer for:

```
WARN regions have no Resource Explorer index yet; resources there are NOT
     reported by this scan ... regions=11 list=ap-northeast-1,ap-south-1,...
```

**To scan a Region immediately, name it.** Explicit regions skip Resource
Explorer entirely and read the EC2 APIs directly, so nothing is missed and
nothing is created:

```bash
tellury scan --aws-account 123456789012 --aws-regions us-west-1
```

**Note that searching writes.** Creating an index is the one non-read-only thing
a scan can do, and it happens only on this path. If that is unacceptable in your
account, pass `--aws-regions` and Resource Explorer is never called.

An empty result from a Region that IS indexed is a real answer: nothing of those
types is there.

```
SUMMARY
--------------------------------------------------------------------------
Scope          o-xxxxxxxxxx
Scanned        41 resources across 3 accounts
Regions        4 regions (resource_explorer)
...
```

The `(resource_explorer)` / `(explicit)` annotation appears after the region
count in both the summary block and the JSON output (`region_source` field).

### Staleness

The Resource Explorer index is eventually consistent. Tagged resources appear
within minutes; untagged resources can take up to two hours. Because hydration
always reads the live EC2 APIs — never the index's cached attributes — a stale
index can only cause a very recently created resource to be missed. It can
never produce a stale attribute or a wrong price.

## Example minimal IAM policy

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
        "pricing:GetProducts",
        "resource-explorer-2:Search",
        "resource-explorer-2:ListIndexes"
      ],
      "Resource": "*"
    }
  ]
}
```

For organization scans, add:

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

## Credentials

tellury reads no key files. It uses the standard AWS SDK credential chain:
environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_SESSION_TOKEN`), the shared credentials file (`~/.aws/credentials`), and
the profile named by `AWS_PROFILE`. Run `aws configure` or set the environment
variables — exactly as you would for the AWS CLI.
