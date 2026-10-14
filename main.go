// Command awsume generates AWS Management Console sign-in URLs from AWS
// CLI profiles, environment credentials, assumed roles and IAM Identity
// Center (SSO) sessions.
package main

import (
	"os"

	"github.com/confused-binary/awsume/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.Run(cli.Options{
		Args:            os.Args[1:],
		Stdin:           os.Stdin,
		StdinIsTerminal: isTerminal(os.Stdin),
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
		Env:             os.Getenv,
		Version:         version,
	}))
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
