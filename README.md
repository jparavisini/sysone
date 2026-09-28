# sysone

[![ci](https://github.com/jparavisini/sysone/actions/workflows/ci.yml/badge.svg)](https://github.com/jparavisini/sysone/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/jparavisini/sysone)](https://github.com/jparavisini/sysone/releases)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Fast, model-agnostic System-1 decision CLI. Send a state (text or JSON) plus typed questions to a System-1 server and get calibrated probabilities back in one forward pass. No text is generated. Classify, route, triage, gate, score, or apply stated rules to a case, from a shell script or an agent.

One static binary, stdlib only, sub-10ms startup. Talks to named endpoints; the model behind an endpoint can change without touching the CLI.

## Install

```bash
d="${SYSONE_INSTALL_DIR:-$HOME/.local/bin}"; t="$(mktemp -d)"; os="$(uname -s | tr '[:upper:]' '[:lower:]')"; a="$(uname -m)"; case "$a" in x86_64) a=amd64;; aarch64|arm64) a=arm64;; esac; gh release download -R jparavisini/sysone -p "sysone_*_${os}_${a}.tar.gz" -p checksums.txt -D "$t" && (cd "$t" && grep "_${os}_${a}.tar.gz" checksums.txt | shasum -a 256 -c - >/dev/null && tar xzf sysone_*.tar.gz) && mkdir -p "$d" && install "$t/sysone" "$d/sysone" && rm -rf "$t" && "$d/sysone" version
```

Needs the [GitHub CLI](https://cli.github.com) (`gh`). Run the same line again to upgrade. Make sure `~/.local/bin` is on your `PATH`.

Other routes: `go install github.com/jparavisini/sysone@latest`, or a tarball from [releases](https://github.com/jparavisini/sysone/releases) (darwin/linux, arm64/amd64, with `checksums.txt`).

## 30-second quickstart

```bash
sysone config init
sysone config set endpoints.default.url https://sysone.example.com
sysone config set endpoints.default.key_cmd "pass show sysone/api-key"   # or: export SYSONE_KEY=...
sysone health

# yes/no gate: exit 0 yes, 1 no, 3 unsure
sysone gate "Is this spam?" < mail.eml && mv mail.eml spam/

# one label
sysone pick "Which team handles this?" billing="invoices, refunds" tech="bugs, outages" other < ticket.txt

# a level on a scale (prints the index)
sysone score "How severe?" "cosmetic" "degraded" "outage" < incident.txt

# every question about one state in a single request
sysone ask -s ticket-triage < ticket.txt | jq .

# many states, JSONL in, JSONL out, input order kept
jq -c '.[]' txns.json | sysone batch -s txn-category --keep -j 4 | jq 'select(.abstain)'
```

STATE is a literal argument, `@file`, `-`, or stdin when omitted and stdin is a pipe.

## Read `p`, not just the answer

Every answer carries `p`, the calibrated probability of the chosen answer. Below the threshold (default 0.9, `--threshold P`) the answer is flagged `"abstain": true`. `pick`, `gate` and `score` then exit **3** and print the best guess to stderr instead of stdout. `ask` and `batch` never exit 3; check `abstain` in the JSON.

An abstain means the model does not know. Escalate: gather more state, reason about the case, or hand it to a person.

```bash
if sysone gate "Is this a refund request?" < msg.txt; then
  handle_refund
elif [ $? -eq 3 ]; then
  escalate msg.txt
fi
```

## Commands

```
sysone ask    [STATE] [--yesno "q"] [--choice "q" --opt a=desc --opt b=desc] [--score "q" --level l0 --level l1]
sysone ask    [STATE] -s SPEC | -q questions.json
sysone pick   [STATE] "instructions" label[=desc] label[=desc] ...     # prints label
sysone gate   [STATE] "yes/no question"                                # exit 0 yes, 1 no, 3 unsure
sysone score  [STATE] "instructions" "level0" "level1" ...             # prints level index
sysone batch  (-s SPEC | -q FILE) [-j N] [--in-field state] [--keep] [--fail-fast]
sysone health [-e ENDPOINT | --all]
sysone spec   list | show NAME | new NAME | path
sysone config init | show | path | edit | set KEY VALUE
sysone skills list | print NAME | install [--agent claude|codex|generic] [--dir PATH] [--force] | uninstall
sysone version
```

Every command has `--help` with examples.

### Output

JSON when stdout is a pipe, human-readable on a terminal. Override with `-o json|jsonl|text|tsv|value`. `value` prints only the bare answer (label, `yes`/`no`, or level index), so `team=$(sysone pick -o value ...)` works. Results go to stdout; diagnostics go to stderr.

One answer, whatever the server returned:

```json
{"id": "team", "type": "choice", "answer": "billing", "p": 0.91,
 "probabilities": {"billing": 0.91, "tech": 0.06, "other": 0.03},
 "abstain": false, "model": "...", "latency_ms": 143}
```

`answer` is a label (choice), `true`/`false` (yesno), or an integer level (score; `expected` carries the expected value when the server sends it). `p` comes from `answer_confidence` when the server sends it, otherwise from `probabilities`, and for yes/no falls back to `confidence`. `--raw` adds the unmodified server answer under `raw`.

`ask` wraps answers: `{"answers": [...], "abstain": bool, "model": "...", "latency_ms": N}`. `batch` prints one such object per input line, plus `"input"` with `--keep`; a failed line becomes `{"error": "...", "line": N}` and the run continues unless `--fail-fast` (auth errors always stop it).

### Global flags

`-e/--endpoint NAME`, `--url`, `--key`, `--model NAME`, `--timeout DUR`, `--threshold P`, `-o FORMAT`, `--raw`, `-v` (log requests to stderr; never the key). They work before or after positionals.

## Exit codes

| code | meaning |
|---|---|
| 0 | ok, or yes (`gate`) |
| 1 | no (`gate`) |
| 2 | usage error |
| 3 | below threshold: abstain (`pick`, `gate`, `score`) |
| 4 | authentication failed |
| 5 | network or server error (after retries) |
| 6 | bad spec or input |

Connection errors, 429, 503, 529 and other 5xx are retried up to 3 times with jittered backoff, honouring `Retry-After`. Other 4xx is never retried.

## Configuration

`~/.config/sysone/config.toml` (respects `$XDG_CONFIG_HOME`; `$SYSONE_CONFIG` points at a specific file). Precedence: flags > environment > config > built-in defaults.

```toml
default_endpoint = "default"

[endpoints.default]
url = "https://sysone.example.com"
key_cmd = "pass show sysone/api-key"     # or key_env = "MY_KEY", key_file = "~/.sysone-key", key = "..."
timeout = "30s"
threshold = 0.9
concurrency = 4                          # default -j for batch
# headers = { "X-Team" = "ops" }
# ca_file = "~/.config/sysone/ca.pem"
# insecure = false

[endpoints.staging]
url = "https://staging.example.com"
key_env = "SYSONE_STAGING_KEY"
```

Auth, first one set wins: `--key`, `$SYSONE_KEY`, then the endpoint's `key`, `key_env`, `key_file` (warns when the file mode is looser than 0600), `key_cmd` (first line of stdout; run once per process). The key is never printed or logged, including by `config show` and `-v`.

Environment: `SYSONE_ENDPOINT`, `SYSONE_URL`, `SYSONE_KEY`, `SYSONE_MODEL`, `SYSONE_TIMEOUT`, `SYSONE_THRESHOLD`, `SYSONE_CONFIG`.

With no config file at all, `SYSONE_URL` and `SYSONE_KEY` are enough.

## Endpoints: local server, OpenRouter, TypeSafe

Any server that speaks the System-1 wire protocol works. Hosted APIs need `model` in the request and use their own path; both are per-endpoint settings. Switch with `-e NAME`.

```toml
[endpoints.local]
url = "http://sysone.internal:8080"
key_cmd = "pass show sysone/local"

[endpoints.openrouter]                  # Jev via OpenRouter, billed per input token
url = "https://openrouter.ai/api"
path = "/alpha/decisions"
model = "typesafe/jev-1.13"             # or "~typesafe/jev-latest"
key_env = "OPENROUTER_API_KEY"

[endpoints.typesafe]                    # Jev direct
url = "https://api.typesafe.ai"
model = "jev-latest"
key_env = "TYPESAFE_API_KEY"
```

```bash
sysone -e openrouter ask -s ticket-triage < ticket.txt | jq '.usage.cost'
sysone -e openrouter --model '~typesafe/jev-latest' gate "Is this spam?" < mail.eml
```

`path` defaults to `/v1/systemone`. `--model` and `SYSONE_MODEL` override the endpoint's `model`. Endpoints without `GET /health` get a one-question probe from `sysone health` instead. When the server reports `usage` (OpenRouter includes `cost` in USD) it passes through on `ask` and `batch` output.

## Specs

A spec is a reusable named question set in `~/.config/sysone/specs/NAME.json`, for `ask -s NAME` and `batch -s NAME`. `sysone config init` installs two examples, `sentiment` and `ticket-triage`; `sysone spec new NAME` writes a template.

```json
{
  "description": "Route and prioritise a support ticket",
  "rules": ["Anything mentioning a chargeback goes to billing, even if it reads as a bug."],
  "questions": {
    "team":    {"type": "choice", "instructions": "Which team should own this ticket?",
                "criteria": {"billing": "invoices, refunds, chargebacks", "tech": "bugs, outages", "other": "anything else"}},
    "urgent":  {"type": "yesno",  "instructions": "Does this need a response within the hour?"},
    "blocked": {"type": "score",  "instructions": "How much is the customer blocked?",
                "criteria": ["not blocked", "workaround exists", "fully blocked"]}
  }
}
```

- `choice`: `criteria` is an object of `label: description`. Write the descriptions; the model reads them. Keep it under 40 options and split hierarchically above that.
- `yesno`: one clear question. (`noul` on the wire; the CLI accepts both.) An optional `criteria` object such as `{"true": "...", "false": "..."}` passes through.
- `score`: `criteria` is an ordered list of level descriptions; the answer is the index of the most probable level. A fractional server score (the probability-weighted position) is kept as `expected`.
- `rules`: optional list merged into the state under `rules`. A text state becomes `{"text": ..., "rules": [...]}`; a JSON object state gets a `rules` key.

`-q FILE` accepts the same shape, or a bare question map. In `batch`, a per-line `"questions"` object overrides the spec for that line.

## Agent skills

Two skills are embedded in the binary: `sysone` (when to use a System-1 decision, exit codes, gating on `p`, batch) and `sysone-specs` (how to write good question sets). Both teach agents to escalate when `p` is below the threshold.

```bash
sysone skills list
sysone skills install                        # ~/.claude/skills/<name>/SKILL.md
sysone skills install --agent codex --dir .  # appends a section to ./AGENTS.md
sysone skills install --agent generic        # prints the snippet
sysone skills uninstall
```

## Wire protocol

`GET /health` (no auth; any 2xx is healthy) and `POST /v1/systemone` with `Authorization: Bearer <key>` and `{"state": ..., "questions": {...}}`. Every question about a state goes in one request. The CLI normalises whatever the server returns; it assumes no field exists.

## License

[MIT](LICENSE)
