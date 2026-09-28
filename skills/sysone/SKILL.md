---
name: sysone
description: Make fast calibrated decisions with the sysone CLI (System-1 server) instead of reasoning or an LLM call. Classify, route, triage, gate, score, or apply stated rules to text or JSON and get a probability back in one request. Triggers: "classify this", "which category", "is this X", "yes or no", "route this", "triage", "score this", "gate on", "batch classify", "sysone". Don't use when the task needs generated text, multi-step reasoning, or facts the state does not contain.
---

# sysone

`sysone` sends a state (text or JSON) plus typed questions to a System-1 server and
returns calibrated probabilities in one forward pass. No text is generated. It is
cheaper and faster than reasoning through the same decision yourself, and its `p`
is calibrated, so you can act on it or escalate.

## When to reach for it

Use `sysone` when the decision is:
- **Closed-form.** The answer is one label from a known set, yes/no, or a level on a scale.
- **Decidable from the state.** Everything needed is in the text or JSON you pass.
- **Repeated.** The same question over many inputs (use `batch`).
- **A gate in a script.** You need an exit code, not prose.

Do not use it for open-ended generation, for chains of reasoning, or when the answer
depends on facts outside the state. Put stated rules in the state (or a spec's `rules`).

## Commands

```bash
sysone gate  [STATE] "Is this spam?"                          # exit 0 yes, 1 no, 3 unsure
sysone pick  [STATE] "Which team?" billing=payments tech=bugs other   # prints label
sysone score [STATE] "Severity" "cosmetic" "degraded" "outage"       # prints level index
sysone ask   [STATE] --yesno "q" --choice "q" --opt a=desc --opt b=desc --score "q" --level "l0" --level "l1"
sysone ask   [STATE] -s SPEC                                  # saved question set
sysone batch -s SPEC -j 4 --keep < lines.jsonl                # JSONL in, JSONL out, order kept
sysone health
```

STATE is a literal, `@file`, or stdin when omitted. Always send every question about
one state in one `ask`; never loop single questions over the same state.

## Read the probability, not just the answer

Every answer carries `p`, the calibrated probability of the chosen answer, and
`abstain: true` when `p` is below the threshold (default 0.9, `--threshold P`).

- `pick`, `gate`, `score` exit **3** and print the best guess to stderr when below threshold.
- `ask` and `batch` never exit 3; check `abstain` in the JSON.

**When `p` is below the threshold, do not act on the answer.** Either reason about the
case yourself, gather more state and ask again, or hand it to a human. A low-`p` answer
is the model telling you it does not know.

```bash
if sysone gate "Is this a refund request?" < msg.txt; then
  handle_refund
elif [ $? -eq 3 ]; then
  escalate_to_human msg.txt      # unsure: never guess
fi
```

## Exit codes

| code | meaning |
|---|---|
| 0 | ok, or yes (gate) |
| 1 | no (gate) |
| 2 | usage error |
| 3 | below threshold: abstain |
| 4 | auth failed |
| 5 | network or server error |
| 6 | bad spec or input |

## Output

JSON on a pipe, text on a terminal. `-o value` prints only the bare answer, so
`team=$(sysone pick -o value "Which team?" a b c < t.txt)` works. `-o json|jsonl|tsv`
for scripts. Shape of one answer:

```json
{"id":"q1","type":"choice","answer":"billing","p":0.91,
 "probabilities":{"billing":0.91,"tech":0.06,"other":0.03},"abstain":false,
 "model":"...","latency_ms":143}
```

## Batch

```bash
jq -c '.[]' txns.json | sysone batch -s txn-category --keep -j 4 | jq 'select(.abstain)'
```

Each input line is a raw string or an object with a `state` field (`--in-field` to
change). Output order matches input order. A failed line becomes `{"error": ...}`;
the run continues unless `--fail-fast`. Route every `abstain: true` line to review.

## Writing questions

See the `sysone-specs` skill. In short: exclusive options with descriptions, fewer
than 40 options, split big taxonomies into two questions, put the rules in the state.
