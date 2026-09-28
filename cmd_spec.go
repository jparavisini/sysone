package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const specUsage = `sysone spec - manage saved question sets

Usage:
  sysone spec list            names and descriptions of saved specs
  sysone spec show NAME       print a spec as JSON
  sysone spec new NAME        write a template spec and print its path
  sysone spec path            print the specs directory

Specs live in ~/.config/sysone/specs/NAME.json:
  {
    "description": "one line",
    "rules": ["Refunds over $500 need a manager."],   # optional, merged into the state
    "questions": {
      "team":   {"type": "choice", "instructions": "Which team?", "criteria": {"billing": "...", "tech": "..."}},
      "urgent": {"type": "yesno",  "instructions": "Does this need action today?"},
      "sev":    {"type": "score",  "instructions": "Severity", "criteria": ["cosmetic", "degraded", "outage"]}
    }
  }

Use with: sysone ask -s NAME < state.txt  ·  sysone batch -s NAME < lines.jsonl

Examples:
  sysone spec new txn-category && $EDITOR "$(sysone spec path)/txn-category.json"
  sysone spec show ticket-triage | jq .questions
`

func cmdSpec(args []string) error {
	fs := newFlagSet("spec", specUsage)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fmt.Fprint(stdout, specUsage)
		return &exitError{code: exitUsage}
	}
	switch pos[0] {
	case "list":
		return specList()
	case "show":
		if len(pos) != 2 {
			return fail(exitUsage, "usage: sysone spec show NAME")
		}
		s, err := loadSpec(pos[1])
		if err != nil {
			return err
		}
		return writeJSONIndent(s)
	case "new":
		if len(pos) != 2 {
			return fail(exitUsage, "usage: sysone spec new NAME")
		}
		return specNew(pos[1])
	case "path":
		fmt.Fprintln(stdout, specsDir())
		return nil
	}
	return fail(exitUsage, "unknown spec subcommand %q\n\n%s", pos[0], specUsage)
}

func specList() error {
	entries, err := os.ReadDir(specsDir())
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "no specs yet in %s; run `sysone config init` for examples\n", specsDir())
		return nil
	}
	if err != nil {
		return fail(exitBadInput, "read specs dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(names)
	for _, n := range names {
		desc := ""
		if s, err := loadSpec(n); err == nil {
			desc = s.Description
			if desc == "" {
				desc = fmt.Sprintf("%d questions", len(s.Questions))
			}
		} else {
			desc = "INVALID: " + err.Error()
		}
		if stdoutTTY {
			fmt.Fprintf(stdout, "%-20s %s\n", n, desc)
		} else {
			fmt.Fprintln(stdout, n)
		}
	}
	return nil
}

const specTemplate = `{
  "description": "describe what this spec decides",
  "rules": [],
  "questions": {
    "category": {
      "type": "choice",
      "instructions": "Which category fits best?",
      "criteria": {
        "a": "describe option a so it excludes b",
        "b": "describe option b so it excludes a"
      }
    },
    "flag": {
      "type": "yesno",
      "instructions": "Ask one clear yes/no question."
    }
  }
}
`

func specNew(name string) error {
	p := specPath(name)
	if _, err := os.Stat(p); err == nil {
		return fail(exitUsage, "%s already exists", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	if err := os.WriteFile(p, []byte(specTemplate), 0o644); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	fmt.Fprintln(stdout, p)
	return nil
}

func writeJSONIndent(v any) error {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
