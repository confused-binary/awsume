# awsume

Turn an AWS CLI profile into an AWS Management Console sign-in URL.

One static Go binary, no dependencies. It replaces the ***aws*** + ***jq*** +
***curl*** federation dance, understands every credential type the CLI does,
and picks the sign-in method that gives the longest console session.

## Install

Requires Go 1.22 or newer. From a clone of this repo:

```
go install .
```

That drops `awsume` (or `awsume.exe`) into Go's bin directory, which the Go
installer already puts on your PATH on every OS. Open a new terminal if the
command is not found.

For a machine without Go, build all targets elsewhere and copy the right file
from `dist/` onto that machine's PATH:

```
make cross
```

On Linux, `-clipboard` needs `wl-copy`, `xclip` or `xsel` and `-open` needs
`xdg-open`. The `aws configure export-credentials` fallback needs AWS CLI v2.

## Usage

```
awsume [flags] [profile ...]
```

Print a URL for a profile:

```
awsume -p prod
```

Open EC2 in us-east-1 in the browser and copy the URL instead of printing it:

```
awsume -p prod -s ec2 -r us-east-1 -c -o
```

Land directly on a resource. Region and partition come from the ARN:

```
awsume -p prod -arn arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0 -o

awsume -p prod -arn arn:aws:iam::123456789012:role/Admin

awsume -p prod -arn arn:aws:s3:::my-bucket/logs/
```

Assume a role on the way in:

```
awsume -p base -role-arn arn:aws:iam::123456789012:role/Admin -external-id abc123

awsume -p base -role-arn arn:aws:iam::123456789012:role/Admin -mfa-serial arn:aws:iam::123456789012:mfa/me -mfa-token 123456
```

Use config and credentials files from somewhere else, for example ones
recovered during an engagement. Only the named files are read:

```
awsume -p audit -credentials-file /mnt/loot/credentials -config-file /mnt/loot/config
```

Many profiles at once. Output is `profile<TAB>url` per line, failures go to
stderr and the rest keep going:

```
aws configure list-profiles | awsume

awsume prod staging dev -json
```

With no profile at all it behaves like the AWS CLI: `AWS_ACCESS_KEY_ID` and
friends, then `AWS_PROFILE`, then `default`, then container and instance
credentials.

`awsume -h` lists every flag. `-list-profiles` and `-list-services` show
what is configured and which `-service` names are known.

## What it handles

Credential sources, resolved natively with the same rules as the AWS CLI:

- IAM user and root access keys
- `role_arn` with `source_profile` or `credential_source`, including
  `external_id`, `mfa_serial` and `duration_seconds`
- `role_arn` with `web_identity_token_file`
- IAM Identity Center profiles (`sso_session` and legacy `sso_start_url`),
  reusing and refreshing the `aws sso login` token cache
- `credential_process`
- environment credentials, EC2 instance metadata (IMDSv2) and ECS/EKS
  container credentials
- anything else, through `aws configure export-credentials` when the AWS CLI
  v2 is installed (`-resolver awscli` forces it, `-resolver native` disables it)

Partitions: commercial, GovCloud and China, inferred from the region, the
identity or the ARN. Every STS, sign-in, console and Identity Center endpoint
can be overridden by flag. Proxies come from `HTTPS_PROXY`.

## How the URL is produced

The federation endpoint only accepts temporary credentials, and different
credential types allow different session lengths. `awsume` identifies the
principal with `sts:GetCallerIdentity`, then tries each applicable method
longest session first and stops at the first success:

| Method | Used for | Session length |
| --- | --- | --- |
| `federation-token` | IAM user keys | up to 36 h (1 h for root) |
| `assume-role` | `-role-arn` | up to 12 h |
| `direct` | role, SSO, instance and container credentials | up to 12 h, then the credential lifetime |
| `sso-portal` | Identity Center profiles with no usable token | the access portal's own session |

Durations step down (36 h, 12 h, 4 h, 1 h, 15 min) until AWS accepts one.
`-duration` caps the request, `-methods` restricts which methods run and `-v`
shows every attempt.

`GetSessionToken` credentials cannot be federated into the console at all;
the tool says so and suggests `-role-arn`. The `GetFederationToken` policy
defaults to allow-all because the federated user otherwise has no
permissions. Override it with `-policy`.

This is the same approach as ***pacu***, ***aws_consoler*** and
***aws-console***, combined.

## Destinations

`-service` takes a friendly name (`ec2`, `s3`, `iam`, `cloudshell`, ...;
see `-list-services`). Unknown names map to `/<name>/home`. A value starting
with `/` is used verbatim as the console path.

`-arn` builds a deep link for the common resources in EC2, VPC, ELB, Auto
Scaling, S3, IAM, Lambda, SQS, SNS, DynamoDB, RDS, CloudFormation,
CloudWatch, Secrets Manager, SSM, KMS, ECS, EKS, ECR, CloudTrail, Step
Functions, EventBridge, CodePipeline, CodeCommit, CodeBuild, Cognito, API
Gateway, ACM, Glue, Redshift, GuardDuty, SageMaker, EFS, Kinesis,
OpenSearch, Route 53, CloudFront and Organizations. Anything else lands on
that service's console home in the ARN's region.

`-destination` takes a full console URL and overrides both.

## JSON output

`-json` prints one object per profile:

```json
{"profile":"prod","url":"https://signin.aws.amazon.com/federation?Action=login&...","method":"direct","session_duration_seconds":43200,"expires":"2026-01-01T12:00:00Z","destination":"https://console.aws.amazon.com/console/home","region":"us-east-1","partition":"aws","account":"123456789012","arn":"arn:aws:sts::123456789012:assumed-role/Admin/me","identity_kind":"assumed-role","credential_source":"assume-role"}
```

`account` and `arn` are omitted with `-no-identity-check`. A `warning` field
appears when an access portal link was returned instead of a federated URL.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | every requested URL was produced |
| 1 | at least one profile failed, or clipboard/open failed |
| 2 | invalid flags or configuration |

## Development

```
make build

make test

make lint

make cross
```
