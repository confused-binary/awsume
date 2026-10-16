package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"awsume/internal/awsconfig"
	"awsume/internal/console"
	"awsume/internal/endpoints"
	"awsume/internal/resolve"
	"awsume/internal/signin"
	"awsume/internal/webdriver"
)

// completeCommand is the hidden first argument the shell completion scripts
// use to ask the binary for candidates:
//
//	awsume __complete <count> [word ...]
//
// The words are everything typed after the command name up to the cursor;
// the last one is the (possibly empty) word being completed. count is the
// number of words the shell meant to pass; missing trailing words are
// treated as empty because some shells drop empty arguments.
const completeCommand = "__complete"

// fileDirective is printed as the last line of __complete output when the
// shell should fall back to file name completion.
const fileDirective = ":file"

// completionShells lists the shells -completion can generate a script for.
var completionShells = []string{"bash", "zsh", "fish", "powershell"}

// complete prints completion candidates, one per line, for the command
// line described by args (see completeCommand). It never fails: an
// unreadable config file simply yields no profile names.
func complete(o Options, args []string) int {
	var words []string
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			words = args[1:]
			for len(words) < n {
				words = append(words, "")
			}
		} else {
			words = args
		}
	}
	if len(words) == 0 {
		words = []string{""}
	}
	cur := words[len(words)-1]

	c := completer{o: o, out: o.Stdout, valueFlags: map[string]bool{}}
	fs := newFlagSet(&flags{}, io.Discard)
	fs.VisitAll(func(fl *flag.Flag) {
		c.flagNames = append(c.flagNames, fl.Name)
		if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
			c.valueFlags[fl.Name] = true
		}
	})
	c.flagNames = append(c.flagNames, "h", "help")
	sort.Strings(c.flagNames)

	// Walk the words before the cursor to pick up the config file flags
	// and to find out whether the current word is a flag's value.
	var pending string
	positionalOnly := false
	for _, w := range words[:len(words)-1] {
		switch {
		case pending != "":
			c.setFile(pending, w)
			pending = ""
		case positionalOnly:
		case w == "--":
			positionalOnly = true
		case strings.HasPrefix(w, "-") && len(w) > 1:
			name := strings.TrimLeft(w, "-")
			if n, v, ok := strings.Cut(name, "="); ok {
				c.setFile(n, v)
			} else if c.valueFlags[name] {
				pending = name
			}
		}
	}

	switch {
	case pending != "":
		c.value(pending, cur, "")
	case !positionalOnly && strings.HasPrefix(cur, "-"):
		dashes := "-"
		if strings.HasPrefix(cur, "--") {
			dashes = "--"
		}
		name := strings.TrimLeft(cur, "-")
		if n, v, ok := strings.Cut(name, "="); ok {
			c.value(n, v, dashes+n+"=")
		} else {
			for _, fl := range c.flagNames {
				if strings.HasPrefix(fl, name) {
					fmt.Fprintln(c.out, dashes+fl)
				}
			}
		}
	default:
		c.emit(c.profiles(), cur, "")
	}
	return ExitOK
}

type completer struct {
	o          Options
	out        io.Writer
	flagNames  []string
	valueFlags map[string]bool

	configPath      string
	credentialsPath string
}

// setFile records -config-file / -credentials-file values seen on the line.
func (c *completer) setFile(name, value string) {
	switch name {
	case "config-file":
		c.configPath = value
	case "credentials-file":
		c.credentialsPath = value
	}
}

// profiles lists the profile names from the files named on the command
// line or, when neither is given, the same defaults the main command uses
// (AWS_CONFIG_FILE, AWS_SHARED_CREDENTIALS_FILE, ~/.aws/...).
func (c *completer) profiles() []string {
	configPath, credentialsPath := c.configPath, c.credentialsPath
	if configPath == "" && credentialsPath == "" {
		configPath, credentialsPath = awsconfig.DefaultPaths(c.o.Env)
	}
	cfg, err := c.o.LoadConfig(configPath, credentialsPath)
	if err != nil {
		return nil
	}
	return cfg.ProfileNames()
}

// value prints the candidates for a flag's value. lead is prepended to
// every candidate (used for the -flag=value form).
func (c *completer) value(name, prefix, lead string) {
	switch name {
	case "config-file", "credentials-file", "chromedriver", "geckodriver", "browser-binary":
		fmt.Fprintln(c.out, fileDirective)
	case "auto-browser", "ab":
		c.emit([]string{webdriver.Chrome, webdriver.Firefox}, prefix, lead)
	case "daemon":
		c.emit(daemonActions, prefix, lead)
	case "profile", "p":
		c.emit(c.profiles(), prefix, lead)
	case "service", "s":
		c.emit(console.KnownServices(), prefix, lead)
	case "resolver":
		c.emit([]string{string(resolve.ModeAuto), string(resolve.ModeNative), string(resolve.ModeAWSCLI)}, prefix, lead)
	case "partition":
		c.emit(endpoints.Names(), prefix, lead)
	case "methods":
		// Comma separated list: complete the element after the last comma.
		done := ""
		if i := strings.LastIndex(prefix, ","); i >= 0 {
			done, prefix = prefix[:i+1], prefix[i+1:]
		}
		c.emit([]string{signin.MethodFederationToken, signin.MethodAssumeRole, signin.MethodDirect, signin.MethodSSOPortal}, prefix, lead+done)
	}
}

func (c *completer) emit(candidates []string, prefix, lead string) {
	for _, cand := range candidates {
		if strings.HasPrefix(cand, prefix) {
			fmt.Fprintln(c.out, lead+cand)
		}
	}
}

// completionScript returns the shell script that wires tab completion for
// shell to the __complete command.
func completionScript(shell string) (string, error) {
	switch strings.ToLower(shell) {
	case "bash":
		return bashCompletion, nil
	case "zsh":
		return zshCompletion, nil
	case "fish":
		return fishCompletion, nil
	case "powershell", "pwsh":
		return powershellCompletion, nil
	}
	return "", fmt.Errorf("unknown shell %q for -completion (want one of %s)", shell, strings.Join(completionShells, ", "))
}

const bashCompletion = `# bash completion for awsume. Load it with:
#   source <(awsume -completion bash)
# or save the output as /etc/bash_completion.d/awsume.
# The script also works when zsh loads it through bashcompinit, so keep it
# free of bash-only syntax: zsh parses a bare name after the second colon in
# ${arr[@]:off:len} as a history modifier, and it has no compopt.
_awsume_complete() {
    local line cur="${COMP_WORDS[COMP_CWORD]}"
    local -a args=("${COMP_WORDS[@]:1:$COMP_CWORD}")
    COMPREPLY=()
    while IFS= read -r line; do
        if [[ "$line" == ":file" ]]; then
            compopt -o default 2>/dev/null || COMPREPLY+=($(compgen -f -- "$cur"))
        elif [[ -n "$line" ]]; then
            COMPREPLY+=("$line")
        fi
    done < <("$1" __complete "$COMP_CWORD" "${args[@]}" 2>/dev/null)
}
complete -F _awsume_complete awsume
`

const zshCompletion = `#compdef awsume
# zsh completion for awsume. Load it with:
#   source <(awsume -completion zsh)
# or save the output as _awsume in a directory on $fpath.
_awsume() {
    local line
    local -a lines cands
    lines=("${(@f)$("${words[1]}" __complete "$((CURRENT - 1))" "${(@)words[2,CURRENT]}" 2>/dev/null)}")
    for line in "${lines[@]}"; do
        if [[ "$line" == ":file" ]]; then
            _files
        elif [[ -n "$line" ]]; then
            cands+=("$line")
        fi
    done
    if (( ${#cands} )); then
        compadd -- "${cands[@]}"
    fi
}
if [[ "${funcstack[1]}" == "_awsume" ]]; then
    _awsume "$@"
else
    compdef _awsume awsume
fi
`

const fishCompletion = `# fish completion for awsume. Load it with:
#   awsume -completion fish | source
# or save the output as ~/.config/fish/completions/awsume.fish.
function __awsume_complete
    set -l tokens (commandline -opc)
    set -l cur (commandline -ct)
    set -l cmd $tokens[1]
    set -e tokens[1]
    set -l n (math (count $tokens) + 1)
    for line in ($cmd __complete $n $tokens "$cur" 2>/dev/null)
        if test "$line" = ":file"
            __fish_complete_path "$cur"
        else if test -n "$line"
            echo $line
        end
    end
end
complete -c awsume -f -a '(__awsume_complete)'
`

const powershellCompletion = `# PowerShell completion for awsume. Load it with:
#   awsume -completion powershell | Out-String | Invoke-Expression
# or add that line to your $PROFILE.
Register-ArgumentCompleter -Native -CommandName 'awsume' -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $exe = $commandAst.CommandElements[0].Extent.Text
    $words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.Extent.Text })
    if ($wordToComplete -eq '') { $words += '' }
    $lines = & $exe '__complete' $words.Count @words 2>$null
    foreach ($line in $lines) {
        if ($line -eq ':file' -or $line -eq '') { continue }
        [System.Management.Automation.CompletionResult]::new($line, $line, 'ParameterValue', $line)
    }
}
`
