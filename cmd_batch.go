package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const batchUsage = `sysone batch - answer the same questions for many states, JSONL in, JSONL out

Usage:
  sysone batch (-s SPEC | -q FILE) [-j N] [--in-field NAME] [--keep] [--fail-fast] < input.jsonl

Each input line is either a raw string (the state) or a JSON object whose
--in-field (default "state") holds the state. A line-level "questions" object
overrides the spec for that line. Output order always matches input order.

Each output line: {"answers": [...], "abstain": bool, "model": "...", "latency_ms": N}
plus "input": {...} with --keep. A failed line becomes {"error": "...", "line": N}.

Flags:
  -s, --spec NAME       saved spec to apply to every line
  -q, --questions FILE  question map or spec file
  -j N                  concurrent requests (default: endpoint concurrency, else 1)
      --in-field NAME   object field holding the state (default "state")
      --keep            copy the input object into the output line under "input"
      --fail-fast       stop at the first error (auth errors always stop)

Examples:
  jq -c '.[]' txns.json | sysone batch -s txn-category --keep -j 4 | jq 'select(.abstain)'
  sysone batch -s sentiment -j 8 < reviews.jsonl | jq -r '.answers[0].answer' | sort | uniq -c
  cat lines.txt | jq -R . | sysone batch -q questions.json --in-field text

Exit codes: 0 all lines processed (errors inline) · 2 usage · 4 auth · 5 network (fail-fast) · 6 bad spec.
`

// batchLine is one unit of work; index keeps output in input order.
type batchLine struct {
	index int
	raw   string
}

type batchOut struct {
	index int
	line  []byte
	err   error
}

func cmdBatch(args []string) error {
	var g globalFlags
	var spec, file, inField string
	var jobs int
	var keep, failFast bool
	fs := newFlagSet("batch", batchUsage)
	g.register(fs)
	fs.StringVar(&spec, "s", "", "spec name")
	fs.StringVar(&spec, "spec", "", "spec name")
	fs.StringVar(&file, "q", "", "questions file")
	fs.StringVar(&file, "questions", "", "questions file")
	fs.IntVar(&jobs, "j", 0, "concurrency")
	fs.StringVar(&inField, "in-field", "state", "state field")
	fs.BoolVar(&keep, "keep", false, "keep input")
	fs.BoolVar(&failFast, "fail-fast", false, "stop on first error")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail(exitUsage, "batch reads JSONL from stdin; unexpected arguments: %s", strings.Join(pos, " "))
	}
	qs, rules, err := questionSource(spec, file)
	if err != nil {
		return err
	}
	if qs == nil {
		return fail(exitUsage, "batch needs -s SPEC or -q FILE")
	}
	ep, err := resolve(&g)
	if err != nil {
		return err
	}
	if jobs < 1 {
		jobs = ep.Concurrency
	}
	c, err := newClient(ep)
	if err != nil {
		return err
	}
	return runBatch(c, qs, rules, inField, keep, failFast, jobs)
}

func runBatch(c *Client, qs *QuestionSet, rules []string, inField string, keep, failFast bool, jobs int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan batchLine)
	out := make(chan batchOut)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l := range in {
				line, err := processLine(ctx, c, qs, rules, inField, keep, l)
				out <- batchOut{index: l.index, line: line, err: err}
			}
		}()
	}

	// Reader: feed lines; bails out when the writer cancels.
	var readErr error
	var total int
	go func() {
		defer close(in)
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			raw := strings.TrimSpace(sc.Text())
			if raw == "" {
				continue
			}
			select {
			case in <- batchLine{index: total, raw: raw}:
				total++
			case <-ctx.Done():
				return
			}
		}
		readErr = sc.Err()
	}()
	go func() { wg.Wait(); close(out) }()

	// Writer: emit in input order, buffering out-of-order results.
	pending := map[int]batchOut{}
	next, done := 0, 0
	var stop error
	for o := range out {
		done++
		pending[o.index] = o
		for {
			r, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if r.err != nil {
				if stop == nil && (failFast || codeOf(r.err) == exitAuth) {
					stop = r.err
					cancel()
				}
				errLine, _ := json.Marshal(map[string]any{"error": r.err.Error(), "line": r.index + 1})
				r.line = errLine
			}
			fmt.Fprintln(stdout, string(r.line))
		}
		if stderrTTY {
			fmt.Fprintf(stderr, "\r%d done", done)
		}
	}
	if stderrTTY {
		fmt.Fprintf(stderr, "\r%d done\n", done)
	}
	if stop != nil {
		return stop
	}
	if readErr != nil {
		return fail(exitBadInput, "read stdin: %v", readErr)
	}
	return nil
}

// processLine parses one input line and asks the server.
func processLine(ctx context.Context, c *Client, qs *QuestionSet, rules []string, inField string, keep bool, l batchLine) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, fail(exitNetwork, "cancelled")
	}
	var state string
	var input map[string]any
	lineQs := qs

	if strings.HasPrefix(l.raw, "{") {
		if err := json.Unmarshal([]byte(l.raw), &input); err != nil {
			return nil, fail(exitBadInput, "line %d: %v", l.index+1, err)
		}
		v, ok := input[inField]
		if !ok {
			return nil, fail(exitBadInput, "line %d: no %q field", l.index+1, inField)
		}
		switch sv := v.(type) {
		case string:
			state = sv
		default:
			b, _ := json.Marshal(sv)
			state = string(b)
		}
		if q, ok := input["questions"]; ok {
			b, _ := json.Marshal(q)
			s, err := parseSpec(b, fmt.Sprintf("line %d", l.index+1))
			if err != nil {
				return nil, err
			}
			lineQs = s.questionSet()
		}
	} else {
		var s string
		if err := json.Unmarshal([]byte(l.raw), &s); err != nil {
			state = l.raw
		} else {
			state = s
		}
	}

	res, err := c.Ask(ctx, stateValue(state, rules), lineQs.IDs, lineQs.Qs)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fail(exitNetwork, "cancelled")
		}
		return nil, err
	}
	rec := map[string]any{
		"answers":    res.Answers,
		"abstain":    res.Abstain,
		"latency_ms": res.LatencyMS,
	}
	if res.Model != "" {
		rec["model"] = res.Model
	}
	if res.Usage != nil {
		rec["usage"] = res.Usage
	}
	if keep {
		if input != nil {
			rec["input"] = input
		} else {
			rec["input"] = state
		}
	}
	return json.Marshal(rec)
}
