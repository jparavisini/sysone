package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// answerString is the bare value for -o value and text output.
func answerString(a *Answer) string {
	switch v := a.Answer.(type) {
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case int:
		return strconv.Itoa(v)
	case string:
		return v
	}
	return fmt.Sprint(a.Answer)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// printResult renders a full ask result in the chosen format.
func printResult(w io.Writer, res *Result, format string) error {
	switch format {
	case "json":
		return writeJSON(w, res)
	case "jsonl":
		for i := range res.Answers {
			if err := writeJSON(w, &res.Answers[i]); err != nil {
				return err
			}
		}
	case "value":
		for i := range res.Answers {
			fmt.Fprintln(w, answerString(&res.Answers[i]))
		}
	case "tsv":
		for i := range res.Answers {
			a := &res.Answers[i]
			fmt.Fprintf(w, "%s\t%s\t%s\t%.4f\t%t\n", a.ID, a.Type, answerString(a), a.P, a.Abstain)
		}
	case "text":
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		for i := range res.Answers {
			a := &res.Answers[i]
			flag := ""
			if a.Abstain {
				flag = "  abstain"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\tp=%.2f%s\t%s\n", a.ID, a.Type, answerString(a), a.P, flag, probsText(a))
		}
		tw.Flush()
		if res.Model != "" || res.LatencyMS > 0 {
			fmt.Fprintf(w, "# %s  %dms\n", res.Model, res.LatencyMS)
		}
	default:
		return fail(exitUsage, "unknown output format %q", format)
	}
	return nil
}

// printSingle renders one answer for pick/gate/score.
func printSingle(w io.Writer, a *Answer, format string) error {
	switch format {
	case "json", "jsonl":
		return writeJSON(w, a)
	case "value":
		fmt.Fprintln(w, answerString(a))
	case "tsv":
		fmt.Fprintf(w, "%s\t%.4f\n", answerString(a), a.P)
	case "text":
		fmt.Fprintf(w, "%s  (p=%.2f)\n", answerString(a), a.P)
	default:
		return fail(exitUsage, "unknown output format %q", format)
	}
	return nil
}

// probsText shows the distribution, highest first, capped at 6 entries.
func probsText(a *Answer) string {
	if len(a.Probabilities) == 0 {
		return ""
	}
	type kv struct {
		k string
		v float64
	}
	var kvs []kv
	for k, v := range a.Probabilities {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v == kvs[j].v {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v > kvs[j].v
	})
	var parts []string
	for i, e := range kvs {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%.2f", e.k, e.v))
	}
	return strings.Join(parts, " ")
}
