---
name: sysone-specs
description: Write good question specs for the sysone CLI (System-1 decisions). Covers choice, yesno and score question design, option descriptions, hierarchical splits, rules in the state, and saving reusable specs under ~/.config/sysone/specs. Triggers: "write a sysone spec", "sysone questions", "classification options", "how do I phrase the question for sysone", "spec for batch", "triage spec". Don't use for running sysone; that is the sysone skill.
---

# sysone-specs

A spec is a reusable question set: `~/.config/sysone/specs/NAME.json`, used with
`sysone ask -s NAME` and `sysone batch -s NAME`. `sysone spec new NAME` writes a
template; `sysone spec show NAME` prints one; `sysone config init` installs the
examples `sentiment` and `ticket-triage`.

```json
{
  "description": "Route a support ticket",
  "rules": ["Anything mentioning a chargeback goes to billing, even if it reads as a bug."],
  "questions": {
    "team":   {"type": "choice", "instructions": "Which team should own this ticket?",
               "criteria": {"billing": "invoices, refunds, chargebacks, plan changes",
                            "tech": "bugs, errors, outages, integrations",
                            "account": "login, password, profile, permissions",
                            "other": "anything not covered above"}},
    "urgent": {"type": "yesno", "instructions": "Does this need a response within the hour?"},
    "sev":    {"type": "score", "instructions": "How much is the customer blocked?",
               "criteria": ["not blocked", "workaround exists", "fully blocked"]}
  }
}
```

## Three question types

- **choice**: one label from `criteria`, an object of `label: description`. The model reads
  the descriptions, so write them.
- **yesno**: one clear question answerable from the state. Wire name is `noul`; write `yesno`.
- **score**: an ordered scale; `criteria` is a list of level descriptions, index 0 first.
  The answer is the level index.

## Rules that make specs work

1. **Exclusive options.** Every state must fit exactly one label. If two labels can both
   be right, merge them or add a rule saying which wins.
2. **Describe every option.** `"tech": "tech"` tells the model nothing. Say what is in and
   what is out: `"tech": "bugs, errors, outages; not billing questions about a bug"`.
3. **Always include an escape label** (`other`, `unclear`, `none`) so the model is not forced
   to pick a wrong fit. Its `p` will tell you how often it is needed.
4. **Fewer than 40 options.** Above that, accuracy drops. Split hierarchically: ask
   `department` first, then a second spec per department.
5. **One decision per question.** "Is it urgent and about billing?" is two questions.
   Ask both; they come back in the same request.
6. **Put stated rules in the state.** Policy the model cannot infer ("orders over $500 need
   a manager") goes in `rules`. The CLI merges it into the state as `rules`, alongside the
   text (`{"text": ..., "rules": [...]}`) or into the JSON object.
7. **Instructions are the question, not a prompt.** No role play, no "you are an expert".
   One sentence saying what to decide.
8. **Scores need a monotone scale.** Each level strictly more than the one before it, and
   the descriptions say what distinguishes adjacent levels.

## Structured state

If the state is a JSON object, pass it as JSON (a file with `@case.json` or a batch line
with `--in-field`). Keep field names meaningful; the model reads them. Strip fields that
do not bear on the decision.

## Check a spec before trusting it

```bash
sysone ask -s NAME < known-case.txt          # does p land where you expect?
sysone batch -s NAME -j 4 < sample.jsonl | jq -r '.answers[] | "\(.p)\t\(.answer)"' | sort -n | head
```

Look at the lowest-`p` answers. Low `p` on cases that seem obvious means the options
overlap or a description is missing. Fix the spec, not the threshold.

## Escalate below threshold

Any answer with `abstain: true` (`p` below the threshold, default 0.9) is not an answer.
Reason about it yourself or hand it to a human. Do not lower the threshold to make
abstains disappear.
