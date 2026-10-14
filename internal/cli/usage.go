package cli

const usage = `awsume - generate AWS Management Console sign-in URLs from CLI profiles

Usage:
  awsume [flags] [profile ...]
  echo profile | awsume [flags]

The profile may be given with -p/-profile, as positional arguments, or piped
on stdin (one profile per line). With no profile the environment
(AWS_ACCESS_KEY_ID..., AWS_PROFILE) and then the "default" profile are used.
The URL is printed on stdout; diagnostics go to stderr.

Where to land:
  -r, -region <region>       Region to open the console in (default: profile region)
  -s, -service <name>        Service console to open, e.g. ec2, s3, iam (see -list-services)
      -arn <arn>             Land on the console page of this resource; its region and
                             partition are used (overrides -region/-service)
  -d, -destination <url>     Explicit console URL to land on (overrides -region/-service)

Output:
  -c, -clipboard             Put the URL on the clipboard instead of printing it
  -o, -open                  Open the URL in the default browser
  -j, -json                  Print a JSON record per profile instead of a bare URL
  -v, -verbose               Explain every step and attempt on stderr

Credentials and session:
  -p, -profile <name>        AWS CLI profile to use
      -config-file <path>    AWS config file (default AWS_CONFIG_FILE, AWS_SHARED_CONFIG_FILE, ~/.aws/config)
      -credentials-file <path> AWS credentials file (default AWS_SHARED_CREDENTIALS_FILE, ~/.aws/credentials)
                             If either flag is given only the named file(s) are read; defaults are not merged in
      -role-arn <arn>        Assume this role before signing in
      -external-id <id>      ExternalId for -role-arn
      -session-name <name>   RoleSessionName for -role-arn
      -mfa-serial <arn>      MFA device for -role-arn (code prompted or via -mfa-token)
      -mfa-token <code>      One-time MFA code for profiles or roles that require MFA
      -role-duration <sec>   DurationSeconds for -role-arn (default 3600)
      -duration <sec>        Cap the console session length that is requested
      -policy <json>         Policy for GetFederationToken (default: allow everything)
      -federation-name <s>   Name for the GetFederationToken user (default awsume)
      -issuer <name>         Issuer shown in the console URL (default awsume)
      -methods <list>        Restrict methods: federation-token,assume-role,direct,sso-portal
      -resolver <mode>       auto (default), native, or awscli (use "aws configure export-credentials")
      -no-identity-check     Skip sts:GetCallerIdentity; guess the identity type instead

Endpoints:
      -partition <id>        aws (default), aws-us-gov or aws-cn (inferred from the region)
      -sts-endpoint <url>    STS endpoint (also AWS_ENDPOINT_URL_STS, AWS_ENDPOINT_URL, profile endpoint_url)
      -signin-endpoint <url> Federation sign-in endpoint (default per partition)
      -console-endpoint <url> Console base URL (default per partition)
      -sso-endpoint <url>    IAM Identity Center portal endpoint
      -sso-oidc-endpoint <url> IAM Identity Center OIDC endpoint

Other:
      -timeout <duration>    Overall timeout (default 2m)
      -no-stdin              Never read profile names from stdin
      -list-profiles         List configured profiles and exit
      -list-services         List known -service names and exit
  -V, -version               Print the version and exit
  -h, -help                  Show this help

Methods are attempted longest session first and the first success wins:
  federation-token  IAM user keys -> sts:GetFederationToken (36h, root 1h) -> sign-in token
  assume-role       -role-arn -> sts:AssumeRole -> sign-in token with SessionDuration (12h)
  direct            role/SSO/instance credentials -> sign-in token with SessionDuration, then
                    with the credential lifetime
  sso-portal        IAM Identity Center access portal deep link (no API call)

Examples:
  awsume -p prod -s ec2 -r us-east-1 -o
  awsume dev -c
  aws configure list-profiles | awsume -json
  awsume -role-arn arn:aws:iam::123456789012:role/Admin -p base
  awsume -p prod -arn arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0 -o
`
