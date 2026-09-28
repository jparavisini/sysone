package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const askUsage = `sysone ask - answer every question about one state in a single request

Usage:
  sysone ask [STATE] --yesno "q" [--choice "q" --opt a=desc --opt b=desc ...] [--score "q" --level "..." ...]
  sysone ask [STATE] -s SPEC
  sysone ask [STATE] -q questions.json

Question flags (repeatable, in any order; ids default to q1..qN):
  --yesno "question"          a yes/no question
  --choice "instructions"     a choice question; follow with --opt label[=description] (2+)
  --score "instructions"      a score question; follow with --level "description" (2+, in order)
  --id NAME                   rename the question declared just before it
  -s, --spec NAME             use a saved spec (~/.config/sysone/specs/NAME.json)
  -q, --questions FILE        read a question map or spec from FILE

Output (default json on a pipe, text on a terminal): {"answers": [...], "abstain": bool,
"model": "...", "latency_ms": N}. Each answer has id, type, answer, p, probabilities,
abstain. p is the calibrated probability of the chosen answer.

Examples:
  sysone ask "The invoice is 40 days overdue." --yesno "Is payment late?" \
    --choice "Which team owns this?" --opt billing=payments --opt support=other requests \
    --score "How urgent?" --level "can wait" --level "this week" --level "today"
  sysone ask -s ticket-triage < ticket.txt | jq '.answers[] | select(.abstain)'
  sysone ask @case.json -q questions.json -o tsv

Exit codes: 0 ok · 2 usage · 4 auth · 5 network/server · 6 bad spec or input.
ask never exits 3; check "abstain" in the output.
`

// askInputs is what every question-taking command resolves before calling
// the server: the merged endpoint, the state and the question set.
type askInputs struct {
	ep     *Resolved
	client *Client
	state  any
	qs     *QuestionSet
	format string
}

// questionSource loads -s / -q into a question set with rules.
func questionSource(spec, file string) (*QuestionSet, []string, error) {
	switch {
	case spec != "" && file != "":
		return nil, nil, fail(exitUsage, "use -s or -q, not both")
	case spec != "":
		s, err := loadSpec(spec)
		if err != nil {
			return nil, nil, err
		}
		return s.questionSet(), s.Rules, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, fail(exitBadInput, "read questions: %v", err)
		}
		s, err := parseSpec(b, file)
		if err != nil {
			return nil, nil, err
		}
		return s.questionSet(), s.Rules, nil
	}
	return nil, nil, nil
}

func cmdAsk(args []string) error {
	var g globalFlags
	var qf questionFlags
	var spec, file string
	fs := newFlagSet("ask", askUsage)
	g.register(fs)
	qf.register(fs)
	fs.StringVar(&spec, "s", "", "spec name")
	fs.StringVar(&spec, "spec", "", "spec name")
	fs.StringVar(&file, "q", "", "questions file")
	fs.StringVar(&file, "questions", "", "questions file")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	format, err := g.outputFormat()
	if err != nil {
		return err
	}

	qs, rules, err := questionSource(spec, file)
	if err != nil {
		return err
	}
	if qs == nil {
		if err := qf.validate(); err != nil {
			return err
		}
		qs = &qf.set
	} else if len(qf.set.IDs) > 0 {
		return fail(exitUsage, "question flags cannot be combined with -s or -q")
	}
	if len(qs.IDs) == 0 {
		return fail(exitUsage, "no questions: use --yesno/--choice/--score, -s SPEC or -q FILE")
	}

	state, rest, err := readState(pos, 0)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fail(exitUsage, "unexpected arguments: %s", strings.Join(rest, " "))
	}

	ep, err := resolve(&g)
	if err != nil {
		return err
	}
	c, err := newClient(ep)
	if err != nil {
		return err
	}
	res, err := c.Ask(context.Background(), stateValue(state, rules), qs.IDs, qs.Qs)
	if err != nil {
		return err
	}
	return printResult(stdout, res, format)
}

const pickUsage = `sysone pick - choose one label for a state

Usage:
  sysone pick [STATE] "instructions" label[=description] label[=description] ...

Prints the chosen label. Below --threshold (default 0.9) it exits 3 and prints the
best guess to stderr instead. Give every option a description: the model reads them.

Examples:
  sysone pick "Reset my password please" "Which team handles this?" \
    billing="payments and refunds" account="login, password, profile" other
  team=$(sysone pick -o value "Which team?" billing account other < ticket.txt) || echo unsure
  sysone pick @review.txt "Sentiment of the review" positive neutral negative -o json

Exit codes: 0 ok · 2 usage · 3 below threshold · 4 auth · 5 network/server · 6 bad input.
`

func cmdPick(args []string) error {
	var g globalFlags
	fs := newFlagSet("pick", pickUsage)
	g.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return fail(exitUsage, "pick needs \"instructions\" and at least 2 labels\n\n%s", pickUsage)
	}
	state, rest, err := readState(pos, 3)
	if err != nil {
		return err
	}
	opts, err := choiceCriteria(rest[1:])
	if err != nil {
		return err
	}
	return single(&g, state, Question{Type: typeChoice, Instructions: rest[0], Criteria: opts}, nil)
}

const gateUsage = `sysone gate - yes/no decision for scripts

Usage:
  sysone gate [STATE] "yes/no question"

Exit 0 for yes, 1 for no, 3 when p is below --threshold (default 0.9). Prints
yes/no on stdout (text or value), or the answer JSON on a pipe.

Examples:
  sysone gate "Is this spam?" < mail.eml && mv mail.eml spam/
  if sysone gate @pr.diff "Does this change touch authentication?"; then notify-security; fi
  sysone gate "Is the customer asking for a refund?" -o value < msg.txt   # yes|no
  sysone gate --threshold 0.75 "Is this urgent?" < ticket.txt; echo "exit $?"

Exit codes: 0 yes · 1 no · 2 usage · 3 unsure · 4 auth · 5 network/server · 6 bad input.
`

func cmdGate(args []string) error {
	var g globalFlags
	fs := newFlagSet("gate", gateUsage)
	g.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fail(exitUsage, "gate needs a \"yes/no question\"\n\n%s", gateUsage)
	}
	state, rest, err := readState(pos, 1)
	if err != nil {
		return err
	}
	return single(&g, state, Question{Type: typeYesNo, Instructions: rest[0]}, func(a *Answer) error {
		if a.Answer == false {
			return &exitError{code: exitNo}
		}
		return nil
	})
}

const scoreUsage = `sysone score - rate a state on an ordered scale

Usage:
  sysone score [STATE] "instructions" "level 0" "level 1" ["level 2" ...]

Prints the index of the chosen level (0-based). Describe each level; the order is
the scale. Below --threshold it exits 3 and prints the best guess to stderr.

Examples:
  sysone score "Site is down for all users" "How severe?" "cosmetic" "degraded" "outage"
  sev=$(sysone score -o value @incident.txt "Severity" "low" "medium" "high" "critical")
  sysone score "Rate the code review comment tone" "hostile" "neutral" "kind" < comment.txt -o json

Exit codes: 0 ok · 2 usage · 3 below threshold · 4 auth · 5 network/server · 6 bad input.
`

func cmdScore(args []string) error {
	var g globalFlags
	fs := newFlagSet("score", scoreUsage)
	g.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return fail(exitUsage, "score needs \"instructions\" and at least 2 levels\n\n%s", scoreUsage)
	}
	state, rest, err := readState(pos, 3)
	if err != nil {
		return err
	}
	levels := make([]any, 0, len(rest)-1)
	for _, l := range rest[1:] {
		levels = append(levels, l)
	}
	return single(&g, state, Question{Type: typeScore, Instructions: rest[0], Criteria: levels}, nil)
}

// single runs one question and prints one answer. after maps the answer to
// an exit code (gate's no → 1). Abstain exits 3 with the guess on stderr.
func single(g *globalFlags, state string, q Question, after func(*Answer) error) error {
	format, err := g.outputFormat()
	if err != nil {
		return err
	}
	ep, err := resolve(g)
	if err != nil {
		return err
	}
	c, err := newClient(ep)
	if err != nil {
		return err
	}
	const id = "q1"
	res, err := c.Ask(context.Background(), stateValue(state, nil), []string{id}, map[string]Question{id: q})
	if err != nil {
		return err
	}
	a := &res.Answers[0]
	if a.Abstain {
		return fail(exitAbstain, "below threshold: best guess %s (p=%.2f < %.2f)", answerString(a), a.P, ep.Threshold)
	}
	if err := printSingle(stdout, a, format); err != nil {
		return err
	}
	if after != nil {
		return after(a)
	}
	return nil
}

const healthUsage = `sysone health - check an endpoint

Usage:
  sysone health [-e ENDPOINT | --all]

Calls GET /health (no auth) and prints latency plus whatever JSON the server
returns (model, version, loaded, ...).

Examples:
  sysone health
  sysone health --all
  sysone health -o json | jq .model

Exit codes: 0 healthy · 5 unreachable or non-2xx (with --all: any endpoint failing).
`

func cmdHealth(args []string) error {
	var g globalFlags
	var all bool
	fs := newFlagSet("health", healthUsage)
	g.register(fs)
	fs.BoolVar(&all, "all", false, "check every configured endpoint")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	format, err := g.outputFormat()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := []string{first(g.endpoint, getenv("SYSONE_ENDPOINT"), cfg.DefaultEndpoint)}
	if all {
		names = names[:0]
		for n := range cfg.Endpoints {
			names = append(names, n)
		}
		sort.Strings(names)
	}

	var failed error
	for _, name := range names {
		gg := g
		gg.endpoint = name
		row := map[string]any{"endpoint": name}
		ep, err := resolveWith(cfg, &gg)
		var body json.RawMessage
		var lat time.Duration
		if err == nil {
			row["url"] = ep.URL
			var c *Client
			if c, err = newClient(ep); err == nil {
				body, lat, err = c.Health(context.Background())
			}
		}
		row["latency_ms"] = lat.Milliseconds()
		if err != nil {
			row["ok"] = false
			row["error"] = err.Error()
			failed = err
		} else {
			row["ok"] = true
			row["health"] = body
		}
		switch format {
		case "text":
			status := "ok"
			if err != nil {
				status = "FAIL " + err.Error()
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%dms\n", name, row["url"], status, lat.Milliseconds())
			if body != nil {
				fmt.Fprintf(stdout, "\t%s\n", strings.TrimSpace(string(body)))
			}
		case "value":
			fmt.Fprintln(stdout, row["ok"])
		default:
			if err := writeJSON(stdout, row); err != nil {
				return err
			}
		}
	}
	if failed != nil {
		if len(names) == 1 {
			return &exitError{code: codeOf(failed)}
		}
		return &exitError{code: exitNetwork}
	}
	return nil
}

// codeOf returns the exit code an error maps to, without printing it.
func codeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return exitUnhandled
}
