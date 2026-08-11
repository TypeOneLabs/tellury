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
| `pricing:GetProducts` | Load the live Price List API catalogue. Optional — when missing, the scan degrades to the embedded static price table and logs a warning. |
| `resource-explorer-2:Search` | Query the aggregator index to find which regions hold resources of the types the selected rules need, so the scan sweeps only those regions instead of every enabled region. Optional — when missing, the scan falls back to `DescribeRegions`. |
| `resource-explorer-2:ListIndexes` | Check whether an aggregator index exists so tellury can report why discovery was available or unavailable. Optional. |

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

## Resource Explorer and region narrowing

When Resource Explorer is available in an account (an aggregator index exists
and the caller has `resource-explorer-2:Search`), tellury queries it to learn
which regions actually hold EBS volumes and Elastic IPs. The scan then hydrates
only those regions — typically 2–4 instead of all 17+ enabled regions — cutting
the per-account API call count from ~35 to ~10.

When Resource Explorer is unavailable (no aggregator index, missing permission,
or an API error), the scan falls back to `ec2:DescribeRegions` and sweeps every
enabled region. This fallback is per-account: in an organization scan, one
account may be narrowed by discovery while its neighbour falls back to the full
sweep.

The scan summary tells you which path each account took:

```
Summary: organizations/o-xxxxxxxxxx — 3 accounts analyzed, 4 regions analyzed (resource_explorer), ...
```

The `(resource_explorer)` / `(describe_regions)` / `(explicit)` annotation
appears after the region count in both the table summary line and the JSON
output (`region_source` field).

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
