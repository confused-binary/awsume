# awsume

Generate AWS Management Console sign-in URLs from AWS CLI profiles.

`awsume` is a single static binary for Linux, macOS and Windows written
in Go with no third party dependencies. It replaces the usual shell pipeline of
`aws configure export-credentials`, `jq` and `curl` against the federation
endpoint, and it knows every way AWS allows credentials to be turned into a
console session, trying the one that yields the longest session first.

## Capabilities

**Credential sources** (the same provider chain as the AWS CLI, implemented
natively):

- long-lived IAM user access keys, and root access keys
- `role_arn` profiles with `source_profile` or `credential_source`
  (`Environment`, `Ec2InstanceMetadata`, `EcsContainer`), including
  `external_id`, `role_session_name`, `duration_seconds` and `mfa_serial`
- `role_arn` with `web_identity_token_file`, and the `AWS_ROLE_ARN` /
  `AWS_WEB_IDENTITY_TOKEN_FILE` environment equivalent
- IAM Identity Center (AWS SSO) profiles, both `sso_session` and legacy
  `sso_start_url` style, reusing the `aws sso login` token cache and
  refreshing expired tokens through the OIDC endpoint
- `credential_process` profiles
- `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` from
  the environment
- EC2 instance metadata (IMDSv2) and ECS/EKS container credentials
- anything else the AWS CLI v2 understands, through an automatic
  `aws configure export-credentials` fallback
- alternate config and credentials files, for example ones recovered during
  an engagement

**Sign-in methods**, planned per identity and tried longest session first:

- `sts:GetFederationToken` for IAM user keys (up to 36 hours)
- `sts:AssumeRole` on the way in, with external id and MFA, then a 12 hour
  console session
- temporary role, SSO, instance or container credentials federated directly
- an IAM Identity Center access portal deep link when no API call is possible

**Where to land**:

- any region, any service console by friendly name, or a literal console path
- a deep link to a specific resource given its ARN, for dozens of resource
  types across more than 30 services
- an explicit destination URL

**Output and automation**:

- print the URL, copy it to the clipboard, open it in a browser, or all three
- one JSON record per profile for scripting
- many profiles at once from arguments or stdin, with per-profile success
  reporting and exit codes
- verbose tracing of the credential chain and every sign-in attempt

**Partitions and endpoints**:

- commercial, GovCloud and China partitions, inferred from the region, the
  caller identity or the ARN
- every STS, sign-in, console and Identity Center endpoint overridable by
  flag, environment variable or profile configuration
- proxy support through the standard `HTTPS_PROXY` variables

## Install

There are no prebuilt releases; build from source. The only build
requirement is Go 1.22 or newer. The binary itself has no runtime
dependencies. Optional extras at runtime: AWS CLI v2 for the
`export-credentials` fallback, and on Linux a clipboard tool (`wl-copy`,
`xclip` or `xsel`) and `xdg-open` for `-clipboard` and `-open`.

### Any OS, into Go's bin directory

From a clone of this repository:

```
go install .
```

This places `awsume` (or `awsume.exe`) in the directory printed by
`go env GOPATH`, under `bin`. Add that directory to your `PATH` if it is not
already there.

### Linux

```
go build -trimpath -ldflags "-s -w" -o awsume .

sudo install -m 0755 awsume /usr/local/bin/awsume
```

For a user-only install replace the second command with:

```
install -m 0755 -D awsume ~/.local/bin/awsume
```

### macOS

```
go build -trimpath -ldflags "-s -w" -o awsume .

sudo install -m 0755 awsume /usr/local/bin/awsume
```

Locally built binaries are not quarantined by Gatekeeper, so no additional
step is needed. On Apple Silicon the build targets `arm64` automatically.

### Windows

In PowerShell:

```
go build -trimpath -ldflags "-s -w" -o awsume.exe .

New-Item -ItemType Directory -Force "$env:LOCALAPPDATA\Programs\awsume"

Copy-Item awsume.exe "$env:LOCALAPPDATA\Programs\awsume\"

[Environment]::SetEnvironmentVariable("Path", "$env:LOCALAPPDATA\Programs\awsume;" + [Environment]::GetEnvironmentVariable("Path", "User"), "User")
```

Open a new terminal for the `PATH` change to take effect.

### Installing on a machine without Go

Build every target on a machine that has Go, then copy the matching file to
the destination and place it on the `PATH` as above:

```
make cross
```

This writes `dist/awsume-linux-amd64`, `dist/awsume-linux-arm64`,
`dist/awsume-darwin-amd64`, `dist/awsume-darwin-arm64`,
`dist/awsume-windows-amd64.exe` and `dist/awsume-windows-arm64.exe`.
Binaries copied to macOS from another machine may need
`xattr -d com.apple.quarantine awsume` before they will run.

## Build

```
go build -o awsume .
```

The Makefile builds a trimmed binary with the version stamped in from
`git describe`:

```
make build

make cross
```

`make cross` writes Linux, macOS and Windows binaries for amd64 and arm64 to
`dist/`. To cross-compile a single target directly:

```
GOOS=windows GOARCH=amd64 go build -o awsume.exe .

GOOS=darwin GOARCH=arm64 go build -o awsume .
```

Run the tests, vet and coverage:

```
make test

make lint

make cover
```

## Usage

```
awsume [flags] [profile ...]
```

Print a URL for a profile:

```
awsume -p prod
```

Land in the EC2 console in a specific region, put the URL on the clipboard
(instead of printing it) and open it:

```
awsume -p prod -s ec2 -r us-east-1 -c -o
```

Land directly on a resource by ARN. The region and partition are taken from
the ARN, so `-region` and `-service` are not needed:

```
awsume -p prod -arn arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0 -o

awsume -p prod -arn arn:aws:iam::123456789012:role/Admin

awsume -p prod -arn arn:aws:s3:::my-bucket/logs/
```

Land on an explicit console URL:

```
awsume -p prod -d https://us-east-1.console.aws.amazon.com/cloudshell/home
```

Profiles can be positional arguments, and flags may follow them:

```
awsume prod -s s3
```

Pipe profile names in from another program; each line is treated as a profile
and the output is `profile<TAB>url` when more than one is produced:

```
aws configure list-profiles | awsume

echo prod | awsume -o
```

Machine readable output:

```
awsume -p prod -json
```

```json
{"profile":"prod","url":"https://signin.aws.amazon.com/federation?Action=login&...","method":"direct","session_duration_seconds":43200,"expires":"2026-01-01T12:00:00Z","destination":"https://console.aws.amazon.com/console/home","region":"us-east-1","partition":"aws","account":"123456789012","arn":"arn:aws:sts::123456789012:assumed-role/Admin/me","identity_kind":"assumed-role","credential_source":"assume-role"}
```

Assume a role on the way in (with optional external id and MFA):

```
awsume -p base -role-arn arn:aws:iam::123456789012:role/Admin -external-id abc123

awsume -p base -role-arn arn:aws:iam::123456789012:role/Admin -mfa-serial arn:aws:iam::123456789012:mfa/me -mfa-token 123456
```

Use different config or credentials files with `-config-file` and
`-credentials-file`. When either flag is given only the named file(s) are
read; the default locations are not merged in. Without the flags the usual
`AWS_CONFIG_FILE` (or its alias `AWS_SHARED_CONFIG_FILE`) and
`AWS_SHARED_CREDENTIALS_FILE` variables are honoured, then `~/.aws/config`
and `~/.aws/credentials`:

```
awsume -p audit -credentials-file /mnt/loot/credentials -config-file /mnt/loot/config
```

With no profile at all the tool behaves like the AWS CLI: `AWS_ACCESS_KEY_ID`
and friends, then `AWS_PROFILE` (or `AWS_DEFAULT_PROFILE`), then the
`default` profile, then container and instance credentials.

Run `awsume -h` for the built-in help, `awsume -list-profiles` to see
configured profiles (in natural sort order, so `prod-9` precedes `prod-10`)
and `awsume -list-services` for the known `-service` names.

## Flags

Every flag accepts a single or double dash. The short forms are listed where
they exist.

### Where to land

| Flag | Meaning |
| --- | --- |
| `-r`, `-region <region>` | Region to open the console in. Defaults to the profile's `region`, then `AWS_REGION` / `AWS_DEFAULT_REGION`. |
| `-s`, `-service <name>` | Service console to open (see `-list-services`). Unknown names map to `/<name>/home`; a value starting with `/` is used as a literal console path. |
| `-arn <arn>` | Land on the console page for this resource. Its region and partition override `-region` and `-service`. Cannot be combined with `-destination`. |
| `-d`, `-destination <url>` | Explicit console URL to land on. Overrides `-region` and `-service`. |

### Output

| Flag | Meaning |
| --- | --- |
| `-c`, `-clipboard` | Put the URL on the clipboard instead of printing it. With several profiles the URLs are joined by newlines. |
| `-o`, `-open` | Open each URL in the default browser. |
| `-j`, `-json` | Print one JSON record per profile instead of a bare URL. Combined with `-clipboard` the record is still printed. |
| `-v`, `-verbose` | Explain every resolution step and sign-in attempt on stderr. |

### Credentials and session

| Flag | Meaning |
| --- | --- |
| `-p`, `-profile <name>` | AWS CLI profile to use. May also be given positionally or on stdin. |
| `-config-file <path>` | AWS config file. |
| `-credentials-file <path>` | AWS credentials file. If either file flag is given only the named file(s) are read. |
| `-role-arn <arn>` | Assume this role before signing in, from whatever credentials the profile yields. |
| `-external-id <id>` | `ExternalId` for `-role-arn`. |
| `-session-name <name>` | `RoleSessionName` for `-role-arn`. Defaults to `awsume`; names are sanitised to the character set and length AWS accepts. |
| `-mfa-serial <arn>` | MFA device for `-role-arn`. The code is prompted on the terminal or taken from `-mfa-token`. |
| `-mfa-token <code>` | One-time MFA code, used for `-mfa-serial` and for profiles whose `mfa_serial` requires one. |
| `-role-duration <sec>` | `DurationSeconds` for `-role-arn` (default 3600, minimum 900). |
| `-duration <sec>` | Cap the console session length requested by every method (minimum 900). |
| `-policy <json>` | Session policy for `GetFederationToken`. Must be a valid JSON document. Default: allow everything. |
| `-federation-name <s>` | `Name` for the `GetFederationToken` user (default `awsume`). |
| `-issuer <name>` | `Issuer` shown in the console sign-in URL (default `awsume`). |
| `-methods <list>` | Comma separated allow-list of `federation-token`, `assume-role`, `direct`, `sso-portal`. |
| `-resolver <mode>` | `auto` (default), `native`, or `awscli`. See Credential resolution. |
| `-no-identity-check` | Skip `sts:GetCallerIdentity` and guess the identity type from whether a session token is present. |

### Endpoints

| Flag | Meaning |
| --- | --- |
| `-partition <id>` | `aws` (default), `aws-us-gov` or `aws-cn`. Inferred from the region, the caller identity ARN or `-arn` when not set. |
| `-sts-endpoint <url>` | STS endpoint. |
| `-signin-endpoint <url>` | Federation sign-in endpoint, including the `/federation` path. |
| `-console-endpoint <url>` | Console base URL. |
| `-sso-endpoint <url>` | IAM Identity Center portal endpoint (`GetRoleCredentials`). |
| `-sso-oidc-endpoint <url>` | IAM Identity Center OIDC endpoint (token refresh). |

### Other

| Flag | Meaning |
| --- | --- |
| `-timeout <duration>` | Overall deadline for all profiles, Go duration syntax (default `2m`). |
| `-no-stdin` | Never read profile names from stdin. |
| `-list-profiles` | List configured profiles and exit. |
| `-list-services` | List known `-service` names and exit. |
| `-V`, `-version` | Print the version and exit. |
| `-h`, `-help` | Show the built-in help. |

## Profiles and stdin

Profiles are processed in this order of precedence:

1. `-profile` and positional arguments, in the order given.
2. Lines on stdin when stdin is not a terminal and `-no-stdin` is not set.
   Blank lines and lines starting with `#` are skipped.
3. The default resolution (environment, `AWS_PROFILE`, `default`,
   container, instance).

Each profile is handled independently. A failure is reported on stderr with
the profile name, the remaining profiles are still processed and the exit
code reflects the overall outcome. With a single profile the bare URL is
printed; with several, each line is `profile<TAB>url`.

## Credential resolution

The native resolver reads the config and credentials files the way the AWS
CLI does: values in the credentials file override the same keys in the
config file, profile and key lookups are case-insensitive, and
`[sso-session name]` and `[services name]` sections are understood.

For a profile the first matching rule wins:

| Profile keys | Behaviour |
| --- | --- |
| `role_arn` + `source_profile` | Resolve the source profile (static keys win over a nested `role_arn` when a profile is its own source; cycles are detected), then `sts:AssumeRole`. |
| `role_arn` + `credential_source` | `Environment`, `Ec2InstanceMetadata` or `EcsContainer` supplies the source credentials, then `sts:AssumeRole`. |
| `role_arn` + `web_identity_token_file` | `sts:AssumeRoleWithWebIdentity`. |
| `sso_session` or `sso_start_url` | IAM Identity Center `GetRoleCredentials` with the cached access token. |
| `aws_access_key_id` / `aws_secret_access_key` | Static credentials, with `aws_session_token` or `aws_security_token` if present. |
| `credential_process` | Run the command through the platform shell and parse its JSON. |

Role assumption honours `role_session_name`, `external_id`,
`duration_seconds` (default 3600) and `mfa_serial`. When `mfa_serial` is set
the code comes from `-mfa-token`, or is prompted on the terminal when stdin
is interactive.

Additional profile keys that are read: `region`, `endpoint_url`, `services`
and `sts_regional_endpoints`.

### IAM Identity Center

The token cache written by `aws sso login` (`~/.aws/sso/cache/<sha1>.json`,
keyed by session name or legacy start URL) is read. An expired access token
is refreshed through the OIDC endpoint when the cache holds a refresh token,
and the refreshed token is written back. If no usable token exists the tool
still returns an access portal deep link with a warning, which works when
the browser already has a portal session; `-methods` without `sso-portal`
turns that into an error instead.

### Instance and container credentials

When the `default` profile does not exist or carries no credentials, and no
environment credentials are set, the tool tries the ECS/EKS container
endpoint (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` or
`AWS_CONTAINER_CREDENTIALS_FULL_URI`, with
`AWS_CONTAINER_AUTHORIZATION_TOKEN` or
`AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`) and then IMDSv2
(`AWS_EC2_METADATA_SERVICE_ENDPOINT` to relocate it,
`AWS_EC2_METADATA_DISABLED=true` to skip it).

### Resolver modes

| `-resolver` | Behaviour |
| --- | --- |
| `auto` | Native chain first. When it fails and `aws` is on the path, `aws configure export-credentials --format process` is run as a fallback and the native error is reported if that also fails. |
| `native` | Built-in chain only; never shells out. |
| `awscli` | Always use the AWS CLI. Useful for exotic profile configurations the native chain does not implement. |

The fallback requires AWS CLI v2 with `export-credentials`; v1 and older v2
builds are detected and reported.

### Environment variables

| Variable | Used for |
| --- | --- |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` (or `AWS_SECURITY_TOKEN`), `AWS_CREDENTIAL_EXPIRATION` | Environment credentials when no profile is given, and `credential_source = Environment`. |
| `AWS_PROFILE`, `AWS_DEFAULT_PROFILE` | Profile to use when none is given. |
| `AWS_CONFIG_FILE` (or `AWS_SHARED_CONFIG_FILE`), `AWS_SHARED_CREDENTIALS_FILE` | File locations. |
| `AWS_REGION`, `AWS_DEFAULT_REGION` | Region when the profile has none. |
| `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_SESSION_NAME` | Web identity role assumption from the environment. |
| `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`, `AWS_CONTAINER_CREDENTIALS_FULL_URI`, `AWS_CONTAINER_AUTHORIZATION_TOKEN`, `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE` | Container credentials. |
| `AWS_EC2_METADATA_SERVICE_ENDPOINT`, `AWS_EC2_METADATA_DISABLED` | Instance metadata. |
| `AWS_ENDPOINT_URL_STS`, `AWS_ENDPOINT_URL`, `AWS_STS_REGIONAL_ENDPOINTS` | STS endpoint selection. |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | Outbound proxy, through the Go standard library. |
| `BROWSER` | Browser launcher on Linux and BSD. |

## How the URL is produced

The federation endpoint (`https://signin.aws.amazon.com/federation`) only
accepts temporary credentials. There are several ways to obtain credentials
it will accept, and they give different maximum session lengths. `awsume`
determines what kind of principal it holds (with `sts:GetCallerIdentity`),
plans every applicable method and duration, sorts them longest session first
and stops at the first success.

| Method | Used for | Session length |
| --- | --- | --- |
| `federation-token` | IAM user access keys | up to 36 h (`GetFederationToken`), 1 h for root |
| `assume-role` | `-role-arn` from any credentials | up to 12 h via `SessionDuration` |
| `direct` | role, SSO, instance and container credentials | up to 12 h via `SessionDuration`, then the credential lifetime |
| `sso-portal` | IAM Identity Center profiles | the access portal's own session; no API call |

Within a method the durations are stepped down (36 h, 12 h, 4 h, 1 h, 15 min)
until AWS accepts one. Errors that indicate the credentials themselves are
rejected (for example `AccessDenied` on `GetFederationToken`) skip the rest of
that method. The `-duration` flag caps what is requested, `-methods` restricts
which methods may run and `-v` prints every attempt.

The identity is classified as an IAM user, the root user, an assumed role, a
federated user, or an IAM user session. `GetSessionToken` credentials cannot
be federated into the console at all; the tool points this out and suggests
`-role-arn`. `GetFederationToken` credentials are federated without a
`SessionDuration`, which the endpoint rejects for them. With
`-no-identity-check` the classification is guessed from the presence of a
session token instead of calling STS.

This mirrors what the Python tools do, combined:

- **pacu** calls `GetFederationToken` with an allow-all policy for user keys,
  uses role credentials directly, then calls `getSigninToken` and builds the
  login URL.
- **aws_consoler** does the same and adds the `SessionDuration` parameter for
  role credentials.
- **aws-console** uses the credentials from the boto3 profile chain (including
  SSO) with the federation endpoint.

The `GetFederationToken` policy defaults to allow-all because the federated
user otherwise has no permissions; override it with `-policy`.

## Destinations

### Services

`-service` accepts these names (`awsume -list-services` prints the same
list):

```
acm apigateway athena backup batch bedrock billing cloudformation cloudfront
cloudshell cloudtrail cloudwatch codebuild codepipeline cognito config
console detective dynamodb ec2 ecr ecs efs eks elasticache elb eventbridge
glue guardduty home iam identitycenter inspector kinesis kms lambda lightsail
logs macie organizations rds redshift route53 s3 sagemaker secretsmanager
securityhub ses sns sqs ssm stepfunctions sts support systems-manager vpc waf
```

Any other name maps to `/<name>/home`, which is correct for most consoles. A
value starting with `/` is used verbatim as the console path, so
`-s /ec2/home#Volumes:` works. In the commercial partition the region is
inserted into the console host name (`us-east-1.console.aws.amazon.com`);
GovCloud and China use a `region=` query parameter.

### Resource deep links

`-arn` parses the ARN, takes the partition and region from it, and builds a
deep link for these resource types:

| Service | Resources |
| --- | --- |
| EC2 and VPC | instance, security group, volume, snapshot, AMI, key pair, launch template, network interface, Elastic IP, VPC, subnet, route table, internet gateway, NAT gateway, VPC endpoint, transit gateway |
| Auto Scaling | group |
| Elastic Load Balancing | load balancer, target group |
| S3 | bucket, prefix, object (region from the profile when the ARN has none) |
| IAM | user, role, group, policy, MFA device, instance profile, SAML and OIDC provider; an STS assumed-role ARN opens the role |
| Lambda | function (with version) |
| SQS, SNS | queue, topic |
| DynamoDB | table |
| RDS | instance, cluster, snapshot, cluster snapshot |
| CloudFormation | stack |
| CloudWatch | log group, alarm |
| Secrets Manager, SSM, KMS | secret, parameter, document, key |
| ECS, EKS, ECR | cluster, service, task, task definition, EKS cluster, repository |
| CloudTrail | trail |
| Step Functions | state machine, execution |
| EventBridge | rule |
| CodePipeline, CodeCommit, CodeBuild | pipeline, repository, project |
| Cognito | user pool |
| API Gateway | REST API |
| ACM | certificate |
| Glue | database, table, job |
| Redshift | cluster |
| GuardDuty | detector (findings page) |
| SageMaker | notebook instance |
| EFS, Kinesis, OpenSearch | file system, stream, domain |
| Route 53, CloudFront, Organizations | hosted zone, distribution, account (global, no region) |

Any other service or resource type lands on that service's console home in
the ARN's region. CloudWatch log group names receive the double encoding the
console expects.

## Endpoints

Partition specific defaults are used for STS, the sign-in endpoint, the
console and the Identity Center endpoints. Each can be overridden:

```
awsume -p gov -partition aws-us-gov

awsume -p lab -sts-endpoint https://sts.lab.example -signin-endpoint https://signin.lab.example/federation -console-endpoint https://console.lab.example
```

| Partition | Sign-in | Console |
| --- | --- | --- |
| `aws` | `signin.aws.amazon.com` | `console.aws.amazon.com` |
| `aws-us-gov` | `signin.amazonaws-us-gov.com` | `console.amazonaws-us-gov.com` |
| `aws-cn` | `signin.amazonaws.cn` | `console.amazonaws.cn` |

The STS endpoint is chosen in this order: `-sts-endpoint`,
`AWS_ENDPOINT_URL_STS`, the profile's `services` section, `AWS_ENDPOINT_URL`,
the profile's `endpoint_url`, then the regional STS endpoint for the region
(or the legacy global endpoint when `sts_regional_endpoints = legacy` or no
region is known). Identity Center endpoints follow the profile's
`sso_region`. Proxies are taken from `HTTPS_PROXY` as usual.

## Clipboard and browser

`-clipboard` uses `clip` on Windows, `pbcopy` on macOS and `wl-copy`, `xclip` or
`xsel` elsewhere. `-open` uses `rundll32` on Windows, `open` on macOS and
`xdg-open` (or `$BROWSER`) elsewhere. Failures of either are reported and set
the exit code without stopping the remaining profiles.

## JSON output

`-json` prints one object per profile with these fields:

| Field | Meaning |
| --- | --- |
| `profile` | The profile name as requested, or `default`. |
| `url` | The sign-in URL. |
| `method` | `federation-token`, `assume-role`, `direct` or `sso-portal`. |
| `session_duration_seconds` | Console session requested; `0` when it follows the credential lifetime, `-1` for `sso-portal`. |
| `expires` | Best known expiry of the console session (RFC 3339, UTC); omitted when unknown. |
| `destination` | Console URL the sign-in lands on. |
| `region` | Region used; omitted when none is known. |
| `partition` | `aws`, `aws-us-gov` or `aws-cn`. |
| `account`, `arn` | From `GetCallerIdentity`; omitted with `-no-identity-check` or when the call fails. |
| `identity_kind` | `iam-user`, `root`, `iam-user-session`, `assumed-role`, `federated-user` or `unknown`. |
| `credential_source` | Where the credentials came from: `static`, `environment`, `assume-role`, `web-identity`, `sso`, `credential-process`, `instance-metadata`, `container`, `aws-cli` or `federation-token`. |
| `warning` | Present when an access portal link was returned instead of a federated URL. |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | every requested URL was produced |
| 1 | at least one profile failed, or clipboard/open failed |
| 2 | invalid flags or configuration |

## Layout

```
main.go                   entry point
internal/cli              flag parsing, stdin handling, output
internal/resolve          credential provider chain (static, role, web identity, SSO, process, IMDS, ECS, aws cli)
internal/signin           method planning and execution
internal/sts              STS Query API client
internal/federation       getSigninToken / login URL
internal/sigv4            request signing
internal/endpoints        partitions and endpoint overrides
internal/console          console destination URLs and resource deep links
internal/arn              ARN parsing
internal/awsconfig        ~/.aws/config and credentials parsing
internal/awscreds         shared credential value type
internal/clipboard        native clipboard helpers
internal/browser          native browser launchers
```
