// sysone is a System-1 decision CLI: it sends a state and typed questions to a
// System-1 server and prints calibrated answers. No text is generated.
//
// The CLI talks to named endpoints from ~/.config/sysone/config.toml and never
// hard-codes a model, port or response quirk.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// Exit codes. Documented in README and every --help.
const (
	exitOK        = 0
	exitNo        = 1
	exitUsage     = 2
	exitAbstain   = 3
	exitAuth      = 4
	exitNetwork   = 5
	exitBadInput  = 6
	exitUnhandled = 7
)

// version is set at release time via -ldflags "-X main.version=...".
// For `go install` builds it falls back to the module version Go embeds.
var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}

// exitError carries an exit code with its message. Every command returns one
// of these (or nil) and main maps it to the process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, a...)}
}

// Swapped out in tests.
var (
	stdout    io.Writer = os.Stdout
	stderr    io.Writer = os.Stderr
	stdin     io.Reader = os.Stdin
	stdoutTTY           = isTerminal(os.Stdout)
	stderrTTY           = isTerminal(os.Stderr)
	stdinTTY            = isTerminal(os.Stdin)
	getenv              = os.Getenv
)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const rootUsage = `sysone - System-1 decision CLI

Send a state (text or JSON) plus typed questions to a System-1 server and get
calibrated probabilities back in one pass. No text is generated.

Usage:
  sysone ask    [STATE] [question flags | -s SPEC | -q questions.json]
  sysone pick   [STATE] "instructions" label[=desc] label[=desc] ...
  sysone gate   [STATE] "yes/no question"
  sysone score  [STATE] "instructions" "level0" "level1" ...
  sysone batch  [-s SPEC | -q FILE] [-j N] [--in-field state] [--keep]
  sysone health [-e ENDPOINT | --all]
  sysone spec   list | show NAME | new NAME | path
  sysone config init | show | path | edit | set KEY VALUE
  sysone skills list | print NAME | install | uninstall
  sysone version

STATE is a literal string, @file, or - for stdin. When STATE is omitted and
stdin is not a terminal, stdin is the state.

Global flags (accepted by every command, before or after positionals):
  -e, --endpoint NAME   endpoint from config (default: default_endpoint)
      --url URL         override the endpoint URL
      --key KEY         override the API key (prefer SYSONE_KEY or key_cmd)
      --timeout DUR     request timeout, e.g. 30s
      --threshold P     abstain below this probability (default 0.9)
  -o, --output FORMAT   json | jsonl | text | tsv | value
                        (default: json when stdout is a pipe, text on a terminal)
      --raw             include the unmodified server answer under "raw"
  -v                    log requests to stderr (never the key)

Environment: SYSONE_ENDPOINT, SYSONE_URL, SYSONE_KEY, SYSONE_TIMEOUT,
SYSONE_THRESHOLD, SYSONE_CONFIG. Precedence: flags > env > config > defaults.

Exit codes:
  0 ok / yes   1 no (gate)   2 usage   3 below threshold (abstain)
  4 auth       5 network or server     6 bad spec or input

Examples:
  sysone gate "Is this spam?" < mail.eml && mv mail.eml spam/
  category=$(sysone pick -o value "Which team?" billing tech other < ticket.txt)
  jq -c '.[]' txns.json | sysone batch -s txn-category --keep -j 4 | jq 'select(.abstain)'

Run "sysone <command> --help" for details on each command.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, rootUsage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "ask":
		err = cmdAsk(rest)
	case "pick":
		err = cmdPick(rest)
	case "gate":
		err = cmdGate(rest)
	case "score":
		err = cmdScore(rest)
	case "batch":
		err = cmdBatch(rest)
	case "health":
		err = cmdHealth(rest)
	case "spec":
		err = cmdSpec(rest)
	case "config":
		err = cmdConfig(rest)
	case "skills":
		err = cmdSkills(rest)
	case "version", "--version":
		fmt.Fprintln(stdout, "sysone", resolveVersion())
	case "help", "-h", "--help":
		fmt.Fprint(stdout, rootUsage)
	default:
		fmt.Fprintf(stderr, "sysone: unknown command %q\n\n%s", cmd, rootUsage)
		return exitUsage
	}
	return exitCode(err)
}

func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(stderr, "sysone:", ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(stderr, "sysone:", err)
	return exitUnhandled
}

// newFlagSet returns a FlagSet whose usage prints the given text and which
// returns flag.ErrHelp (exit 0) on -h/--help.
func newFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stdout, usage) }
	return fs
}

// parseArgs parses flags interspersed with positionals, so
// `sysone pick "q" a b -o value` works with the stdlib flag package.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fail(exitUsage, "%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// globalFlags are accepted by every command.
type globalFlags struct {
	endpoint  string
	url       string
	key       string
	timeout   string
	threshold float64
	output    string
	raw       bool
	verbose   bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.endpoint, "e", "", "endpoint name")
	fs.StringVar(&g.endpoint, "endpoint", "", "endpoint name")
	fs.StringVar(&g.url, "url", "", "endpoint URL")
	fs.StringVar(&g.key, "key", "", "API key")
	fs.StringVar(&g.timeout, "timeout", "", "request timeout")
	fs.Float64Var(&g.threshold, "threshold", -1, "abstain threshold")
	fs.StringVar(&g.output, "o", "", "output format")
	fs.StringVar(&g.output, "output", "", "output format")
	fs.BoolVar(&g.raw, "raw", false, "include raw server answer")
	fs.BoolVar(&g.verbose, "v", false, "verbose")
}

// outputFormat resolves -o, defaulting on whether stdout is a terminal.
func (g *globalFlags) outputFormat() (string, error) {
	switch g.output {
	case "":
		if stdoutTTY {
			return "text", nil
		}
		return "json", nil
	case "json", "jsonl", "text", "tsv", "value":
		return g.output, nil
	}
	return "", fail(exitUsage, "unknown output format %q (json|jsonl|text|tsv|value)", g.output)
}

// readState resolves the STATE positional: literal, @file, - or stdin.
// need is the number of positionals the command requires after the state;
// when more than that are present, the first one is the state. Otherwise the
// state comes from stdin when stdin is not a terminal.
func readState(pos []string, need int) (state string, rest []string, err error) {
	if len(pos) > need {
		s, rest := pos[0], pos[1:]
		switch {
		case s == "-":
			b, err := io.ReadAll(stdin)
			if err != nil {
				return "", nil, fail(exitBadInput, "read stdin: %v", err)
			}
			return string(b), rest, nil
		case strings.HasPrefix(s, "@"):
			b, err := os.ReadFile(s[1:])
			if err != nil {
				return "", nil, fail(exitBadInput, "read state file: %v", err)
			}
			return string(b), rest, nil
		}
		return s, rest, nil
	}
	if !stdinTTY {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", nil, fail(exitBadInput, "read stdin: %v", err)
		}
		if len(b) > 0 {
			return string(b), pos, nil
		}
	}
	return "", nil, fail(exitUsage, "no state given: pass STATE, @file, - or pipe it on stdin")
}
