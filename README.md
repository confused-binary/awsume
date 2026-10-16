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
`-auto-browser` needs ***chromedriver*** for Chrome or ***geckodriver*** for
Firefox on the PATH (or `-chromedriver` / `-geckodriver` pointing at them),
the same drivers ***waf-fu*** uses.

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

Open the console in a browser that awsume controls and keep the session
alive for as long as that browser is open:

```
awsume -p prod -ab firefox

awsume -p prod -ab chrome -s iam -refresh 10m
```

Keep those browsers in a background daemon instead, so the command returns
at once and the terminal stays free (`-daemon install` makes it a login
service):

```
awsume -daemon start

awsume -p prod -ab chrome
```

Send everything through a proxy, including the browser:

```
awsume -p prod -proxy 127.0.0.1:8080 -k -ab chrome

awsume -p prod -proxy socks5://127.0.0.1:1080
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

## Tab completion

`-completion` prints a completion script for bash, zsh, fish or PowerShell.
To install it, add the matching line to your shell's startup file
(`~/.bashrc`, `~/.zshrc`, `~/.config/fish/config.fish` or your PowerShell
`$PROFILE`) and open a new shell. Running the line directly loads it for the
current shell only:

```
source <(awsume -completion bash)

source <(awsume -completion zsh)

awsume -completion fish | source

awsume -completion powershell | Out-String | Invoke-Expression
```

The zsh script is the one to use in zsh. If your `~/.zshrc` loads bash
completions through `bashcompinit`, the bash script works there as well.

Profile names come from the same files the command would use: the
`-config-file` and `-credentials-file` already typed on the line, otherwise
`AWS_CONFIG_FILE` / `AWS_SHARED_CREDENTIALS_FILE`, otherwise `~/.aws`. Flag
names, `-service`, `-partition`, `-resolver`, `-methods` and `-auto-browser`
values are completed too, and the file and executable flags fall back to
path completion.

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
can be overridden by flag.

## Proxy

`-proxy` takes `http://host:port`, `https://host:port` or
`socks5://host:port`; a bare `host:port` means HTTP. Everything goes through
it: the STS, sign-in and Identity Center calls awsume makes itself, the
`aws configure export-credentials` and `credential_process` subprocesses
(through `HTTPS_PROXY` and friends in their environment) and the browser
started by `-auto-browser`. `-k` / `-insecure` turns off TLS certificate
verification for awsume and the browser, which an intercepting proxy such as
***Burp*** needs unless its CA is trusted. Proxy credentials in the URL work
for awsume's own calls but cannot be handed to a browser.

Without `-proxy` the usual `HTTPS_PROXY` environment applies to awsume's own
calls only.

## Browser session

`-ab` / `-auto-browser chrome|firefox` opens the sign-in URL in a browser
started through ***chromedriver*** or ***geckodriver***, the way
***waf-fu*** replays requests, and then stays in the foreground. The browser
runs with awsume's own profile, kept under `~/.cache/awsume/browser/` (or
`XDG_CACHE_HOME`), so nothing from your daily browser leaks into the
engagement and vice versa, while the console still remembers client-side
preferences between runs: dismiss the navigation tour once and it stays
dismissed. When that profile is already open in another awsume browser, the
new one gets a throwaway profile instead. The URL is still printed (or
copied with `-c`).

Every 15 minutes (`-refresh`) awsume resolves the credentials again, builds
a new sign-in URL and applies it to the browser from inside the open console
tab, so the console cookies are replaced without a new tab or window. It
tries the quietest mechanism first and checks after each one whether the
console cookies actually changed:

1. a background `fetch` of the sign-in URL from the console page, which
   follows the sign-in redirects and stores their cookies;
2. a hidden iframe, a real navigation that also runs any HTML or script
   step of the sign-in flow;
3. with Chrome, a DevTools target that loads the URL in the background
   without taking focus and is closed once the console is reached;
4. a new tab that is closed again as soon as the sign-in has landed.

Steps 3 and 4 only run when the earlier ones fail, and step 4 also when no
console tab is open at all; `-v` shows which step was used and why the
earlier ones did not work. A failed refresh is reported on stderr and
retried a minute later. When a session would expire before the next
refresh, the refresh is brought forward.

Role sessions keep the fixed session name `awsume` (or `-session-name`) so
the console sees the same identity on every refresh.

Closing the browser ends awsume. Ctrl-C ends awsume and closes the
browser, because the browser belongs to the driver session. One profile at
a time: `-auto-browser` refuses multiple profiles.

`-chromedriver`, `-geckodriver` and `-browser-binary` point at executables
when the automatic search (PATH, then the usual install directories) picks
the wrong one.

### Daemon

Run in the foreground, `-auto-browser` occupies the terminal for as long as
the browser is open. The daemon takes that job over: one long-running
awsume owns every browser, and each `-auto-browser` command just hands its
session to it and returns.

```
awsume -daemon start

awsume -p prod -ab chrome

awsume -p staging -ab firefox -s iam

awsume -daemon status

awsume -daemon stop
```

`-daemon start` launches `awsume -daemon run` detached from the terminal,
logging to `~/.cache/awsume/daemon.log` (or `XDG_CACHE_HOME`). While it is
running, every `-auto-browser` command resolves the credentials and builds
the first sign-in URL itself, so MFA prompts, `aws sso login` hints and
error messages still appear in your terminal and the URL is still printed
(or copied with `-c`), and then passes the command line and environment to
the daemon over a Unix socket that only your user can reach. The daemon
starts the browser, loads the URL and refreshes the session from then on
exactly as the foreground mode would, using the same files, proxy and
driver flags, and your `PATH` and display variables, so it also works when
the daemon was started by a login service with a bare environment.

`-daemon status` lists the sessions with how long each has been open, how
many refreshes it has had and the last thing it reported; `-json` prints
the same as a record. `-daemon stop` closes every browser and ends the
daemon. Closing a browser ends just that session. `-no-daemon` keeps a
session in the foreground even when the daemon is running.

Refreshes inside the daemon cannot prompt: a role that needs a fresh MFA
code on every `sts:AssumeRole` only refreshes for as long as the first
code's session lasts, after which the daemon reports the failure in its log
and `-daemon status` and retries every minute; an expired Identity Center
token recovers on its own once you run `aws sso login` again.

The daemon can be installed as a per-user login service, which is the only
kind that can put a browser on your desktop:

```
awsume -daemon install

awsume -daemon uninstall
```

On Linux that writes a systemd user unit (`~/.config/systemd/user/awsume.service`)
and enables it with `systemctl --user enable --now`; its output is in
`journalctl --user -u awsume`. On macOS it writes a launchd agent
(`~/Library/LaunchAgents/com.awsume.daemon.plist`) and loads it with
`launchctl bootstrap`, logging to `~/.cache/awsume/daemon.log`. On Windows
it drops a script into the Startup folder that runs the daemon hidden at
logon, logging to `%LOCALAPPDATA%\awsume\daemon.log`, and starts it right
away. The socket path is baked into the service so clients and daemon
always agree; set `AWSUME_DAEMON_SOCKET` before installing to put it
somewhere else. `-daemon run` runs the daemon in the foreground for a
service manager of your own.

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
