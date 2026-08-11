# AWS setup

## What works today

`--aws-account` only: one account, using the credentials you give it. Organization and
organizational-unit scopes are accepted by the CLI but fail with a message saying they
arrive with Organizations traversal, and there is no cross-account role assumption yet — so
the credentials must belong to the account you are scanning.

Two rules ship: `unattached_ebs_volume` and `unassociated_eip`. Neither needs CloudWatch.

## Authentication

`tellury` reads the standard AWS credential chain — environment variables, the shared
credentials file, `AWS_PROFILE`, and an instance role — exactly as the AWS CLI does. There
is no credentials flag and no key file of tellury's own.

```bash
export AWS_PROFILE=tellury
./tellury scan --aws-account 123456789012
```

With no usable credentials the scan fails immediately, naming what is missing. It does not
fall through to the EC2 instance metadata service and wait for that to time out.

## Permissions

Three read-only actions. That is the whole surface this build calls:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "TelluryReadOnlyScan",
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeRegions",
      "ec2:DescribeVolumes",
      "ec2:DescribeAddresses"
    ],
    "Resource": "*"
  }]
}
```

`ReadOnlyAccess` also works but grants far more than tellury uses.

### Creating a least-privilege user

Run these as an administrator. They create a user that can read what tellury scans and
nothing else.

```bash
aws iam create-policy --policy-name TelluryReadOnlyScan \
  --policy-document file://tellury-policy.json

aws iam create-user --user-name tellury-scanner

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
aws iam attach-user-policy --user-name tellury-scanner \
  --policy-arn "arn:aws:iam::${ACCOUNT_ID}:policy/TelluryReadOnlyScan"

# Write the key straight into a profile so the secret is never printed.
CREDS=$(aws iam create-access-key --user-name tellury-scanner \
          --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text)
aws configure set aws_access_key_id     "$(printf '%s' "$CREDS" | cut -f1)" --profile tellury
aws configure set aws_secret_access_key "$(printf '%s' "$CREDS" | cut -f2)" --profile tellury
aws configure set region eu-west-1 --profile tellury
unset CREDS
```

Two things worth doing afterwards. IAM is eventually consistent, so a new key can take a few
seconds to work — retry rather than assuming it failed. And confirm the policy is not wider
than intended by checking that something outside it is refused:

```bash
aws ec2 describe-regions   --profile tellury --query 'length(Regions)' --output text  # a number
aws ec2 describe-instances --profile tellury                                          # AccessDenied
```

A read-only policy that quietly grants more than you meant is worth catching, and the only
way to see it is to assert the denial.

Prefer IAM Identity Center (`aws configure sso`) if you have it — short-lived credentials
beat a long-lived access key. Do not create root access keys for this.

## Regions

AWS resources are regional and there is no global inventory API, so a scan sweeps regions:

```
ec2:DescribeRegions          the regions THIS account has enabled
  then, per region:          DescribeVolumes + DescribeAddresses
```

That is roughly `1 + 2N` calls for N enabled regions — about 35 for a typical account, and
around 15 seconds. `AllRegions=false`, so regions the account has never opted into are never
queried.

Narrow it when you know where things are:

```bash
./tellury scan --aws-account 123456789012 --aws-regions eu-west-1,eu-central-1
```

An availability-zone spelling like `eu-west-1a` is accepted and flattened to its region.

Every scan reports the regions it actually covered, so an empty result is distinguishable
from a scan that never looked:

```
Summary: accounts/123456789012 — 1 account analyzed, 17 regions analyzed, 2 resources
scanned, 2 rules evaluated, 1 finding, 1 resource skipped, 15.285s
```

## Pricing

Prices come from an embedded table, not the live AWS Price List API. The live catalogue
exists in `pkg/pricing/aws/catalog.go` but is not yet wired to the provider, so every AWS
figure currently reads `price_source = embedded`.

The table is dated in the file and covers what the two rules need: EBS capacity, IOPS and
throughput per volume type, and the hourly rate for an unassociated address.

**Reconcile a finding against your bill before trusting a total.** An embedded table drifts,
and every pricing defect in this project so far — three of them — was found that way and by
nothing else. Two were invisible to the entire test suite; one was a test asserting the wrong
arithmetic, so the suite was green *because* it agreed with the bug.

### Included allowances

EBS bills only provisioning **above** what a volume type includes, and a volume reports its
total provisioning either way:

| Type | Included | Billed above it |
|---|---|---|
| `gp3` | 3000 IOPS, 125 MiB/s | IOPS and throughput |
| `io1`, `io2` | none | every provisioned IOPS |
| `gp2`, `st1`, `sc1`, `standard` | — | no separate IOPS or throughput charge |

This matters more than it looks: every gp3 volume reports 3000 IOPS and 125 MiB/s whether
you chose them or not, so pricing the raw figures adds about $20 a month of cost that does
not exist, to every gp3 volume. The AWS price list does not encode the allowance — its
dimension reads "per provisioned IOPS-month" — so it cannot be derived from the API alone.

## Testing against real resources

To see both rules fire, create two resources and delete them the same day:

```bash
aws ec2 allocate-address --domain vpc --region eu-west-1          # ~$3.65/month
aws ec2 create-volume --size 200 --volume-type gp3 \
  --availability-zone eu-west-1a --region eu-west-1               # ~$16/month
```

`unattached_ebs_volume` requires a volume to have been unattached for **7 days**, which
suppresses the churn of volumes freed moments ago. To exercise it immediately, move the
evaluation instant instead of waiting:

```bash
./tellury scan --aws-account 123456789012 --aws-regions eu-west-1 \
  --at "$(date -u -d '+8 days' +%Y-%m-%dT%H:%M:%SZ)"
```

Then release the address and delete the volume — an unassociated Elastic IP bills whether or
not anything uses it, which is the entire point of the rule.
