package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is a saved question set: ~/.config/sysone/specs/<name>.json.
// Rules are merged into the state under "rules" so the model applies them.
type Spec struct {
	Description string              `json:"description,omitempty"`
	Rules       []string            `json:"rules,omitempty"`
	Questions   map[string]Question `json:"questions"`
}

// QuestionSet is an ordered question map: ids keep the order they were
// declared in, so output is stable.
type QuestionSet struct {
	IDs []string
	Qs  map[string]Question
}

func (s *QuestionSet) add(id string, q Question) {
	if s.Qs == nil {
		s.Qs = map[string]Question{}
	}
	if _, dup := s.Qs[id]; !dup {
		s.IDs = append(s.IDs, id)
	}
	s.Qs[id] = q
}

// specPath resolves a spec name to a file: a bare name lives in the specs
// dir; anything with a path separator or .json suffix is used as given.
func specPath(name string) string {
	if strings.ContainsRune(name, filepath.Separator) || strings.HasSuffix(name, ".json") {
		return name
	}
	return filepath.Join(specsDir(), name+".json")
}

func loadSpec(name string) (*Spec, error) {
	b, err := os.ReadFile(specPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(exitBadInput, "spec %q not found (%s); see `sysone spec list`", name, specPath(name))
	}
	if err != nil {
		return nil, fail(exitBadInput, "read spec: %v", err)
	}
	return parseSpec(b, name)
}

// parseSpec accepts either a full spec {"questions": {...}, "rules": [...]}
// or a bare question map {"q1": {...}}.
func parseSpec(b []byte, name string) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil || s.Questions == nil {
		var bare map[string]Question
		if err2 := json.Unmarshal(b, &bare); err2 != nil || len(bare) == 0 {
			if err == nil {
				err = errors.New("no \"questions\" object")
			}
			return nil, fail(exitBadInput, "spec %s: %v", name, err)
		}
		s = Spec{Questions: bare}
	}
	for id, q := range s.Questions {
		if q.Type == wireYesNo {
			q.Type = typeYesNo
			s.Questions[id] = q
		}
		if err := validateQuestion(id, q); err != nil {
			return nil, fail(exitBadInput, "spec %s: %v", name, err)
		}
	}
	return &s, nil
}

func validateQuestion(id string, q Question) error {
	if strings.TrimSpace(q.Instructions) == "" {
		return fmt.Errorf("question %q: instructions are empty", id)
	}
	switch q.Type {
	case typeYesNo:
		return nil
	case typeChoice:
		opts, ok := q.Criteria.(map[string]any)
		if !ok || len(opts) < 2 {
			return fmt.Errorf("question %q: choice needs a criteria object with at least 2 labels", id)
		}
		if len(opts) >= 40 {
			return fmt.Errorf("question %q: %d options; keep it under 40 and split hierarchically", id, len(opts))
		}
	case typeScore:
		levels, ok := q.Criteria.([]any)
		if !ok || len(levels) < 2 {
			return fmt.Errorf("question %q: score needs a criteria list with at least 2 levels", id)
		}
	default:
		return fmt.Errorf("question %q: unknown type %q (choice|yesno|score)", id, q.Type)
	}
	return nil
}

func (s *Spec) questionSet() *QuestionSet {
	ids := make([]string, 0, len(s.Questions))
	for id := range s.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	qs := &QuestionSet{}
	for _, id := range ids {
		qs.add(id, s.Questions[id])
	}
	return qs
}

// stateValue sends a JSON object as-is and everything else as a string,
// merging spec rules under "rules" when present.
func stateValue(state string, rules []string) any {
	var obj map[string]any
	trimmed := strings.TrimSpace(state)
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &obj) == nil {
		if len(rules) > 0 {
			obj["rules"] = rules
		}
		return obj
	}
	if len(rules) > 0 {
		return map[string]any{"text": state, "rules": rules}
	}
	return state
}

// questionFlags collects the repeatable --yesno/--choice/--opt/--score/--level
// flags for `ask` in declaration order. --opt and --level attach to the
// question declared before them; --id renames it.
type questionFlags struct {
	set     QuestionSet
	current string
	n       int
}

type qFlag struct {
	f  *questionFlags
	fn func(string) error
}

func (q qFlag) String() string     { return "" }
func (q qFlag) Set(v string) error { return q.fn(v) }

func (f *questionFlags) start(t, instructions string) error {
	f.n++
	id := fmt.Sprintf("q%d", f.n)
	q := Question{Type: t, Instructions: instructions}
	switch t {
	case typeChoice:
		q.Criteria = map[string]any{}
	case typeScore:
		q.Criteria = []any{}
	}
	f.set.add(id, q)
	f.current = id
	return nil
}

func (f *questionFlags) last(kind string) (Question, error) {
	if f.current == "" {
		return Question{}, fmt.Errorf("--%s must follow a --choice or --score", kind)
	}
	return f.set.Qs[f.current], nil
}

func (f *questionFlags) register(fs *flag.FlagSet) {
	fs.Var(qFlag{fn: func(v string) error { return f.start(typeYesNo, v) }}, "yesno", "yes/no question")
	fs.Var(qFlag{fn: func(v string) error { return f.start(typeChoice, v) }}, "choice", "choice question")
	fs.Var(qFlag{fn: func(v string) error { return f.start(typeScore, v) }}, "score", "score question")
	fs.Var(qFlag{fn: func(v string) error {
		q, err := f.last("opt")
		if err != nil {
			return err
		}
		opts, ok := q.Criteria.(map[string]any)
		if !ok {
			return errors.New("--opt only follows --choice")
		}
		label, desc, _ := strings.Cut(v, "=")
		if desc == "" {
			desc = label
		}
		opts[label] = desc
		return nil
	}}, "opt", "choice option label[=description]")
	fs.Var(qFlag{fn: func(v string) error {
		q, err := f.last("level")
		if err != nil {
			return err
		}
		levels, ok := q.Criteria.([]any)
		if !ok {
			return errors.New("--level only follows --score")
		}
		q.Criteria = append(levels, v)
		f.set.Qs[f.current] = q
		return nil
	}}, "level", "score level description, in order")
	fs.Var(qFlag{fn: func(v string) error {
		if f.current == "" {
			return errors.New("--id must follow a question flag")
		}
		q := f.set.Qs[f.current]
		delete(f.set.Qs, f.current)
		for i, id := range f.set.IDs {
			if id == f.current {
				f.set.IDs[i] = v
			}
		}
		f.set.Qs[v] = q
		f.current = v
		return nil
	}}, "id", "id for the preceding question")
}

// validate checks every flag-built question.
func (f *questionFlags) validate() error {
	for _, id := range f.set.IDs {
		if err := validateQuestion(id, f.set.Qs[id]); err != nil {
			return fail(exitUsage, "%v", err)
		}
	}
	return nil
}

// choiceCriteria builds a choice criteria map from label[=desc] args.
func choiceCriteria(args []string) (map[string]any, error) {
	opts := map[string]any{}
	for _, a := range args {
		label, desc, _ := strings.Cut(a, "=")
		if label == "" {
			return nil, fail(exitUsage, "empty option label in %q", a)
		}
		if desc == "" {
			desc = label
		}
		opts[label] = desc
	}
	if len(opts) < 2 {
		return nil, fail(exitUsage, "pick needs at least 2 options")
	}
	return opts, nil
}
