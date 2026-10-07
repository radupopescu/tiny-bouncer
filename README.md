# Tiny Bouncer

Tiny Bouncer screens shell commands requested by LLM agents before they run. It sends each
command to an external judgment backend — TypeSafe's **Jev** by default — and turns the
verdict into a permission decision in the [OpenCode V2](https://opencode.ai/v2/docs/)
harness: allow the command, block it, or fall back to the normal interactive prompt.

> **Status: v0.1.0.** All planned tasks are complete (see `doc/plan.md`); the live Jev
> operating point is calibrated and gated (`FNR = 0` hard), and the optional `decider`,
> `api` and `afm` backends are measured and documented as **comparison-only** (see below).
> Manual user smoke test pending under way of working notes.

Design principles:

- **Fail safe to `ask`.** If the classifier is unreachable, times out, or misbehaves,
  the agent is asked interactively — never silently auto-run.
- **The classifier only raises strictness.** Explicitly denied commands in your
  `opencode.json(c)` stay denied; the classifier never grants permission on its own.
- **Backend-agnostic contract.** Verdicts (`allow` / `deny` / `ask`) are generic, so
  other judgment backends can be added without touching the CLI contract or the plugin.

## How it works

```text
agent proposes shell command(s)                  OpenCode V2
        │                                             ▲
        ▼                                             │ allow / ask / deny
permission.evaluate hook ──▶ tinybouncer check ──▶ Jev judgment (probabilities)
                             (one process per   ▲         │
                              permission event) └─ thresholds (yours, in code)
```

- The OpenCode plugin intercepts shell permission checks, batches the commands it was
  given, and spawns `tinybouncer check` once per event.
- `tinybouncer` normalises, optionally caches, calls the selected backend, and applies
  your thresholds to convert probabilities into an effect.
- The plugin maps that effect onto the OpenCode permission decision: a classifier
  `deny` blocks with a reason, an `ask` surfaces the interactive prompt, and an `allow`
  leaves your configured permission decision untouched.

## Quickstart

```sh
# 1. Build the classifier
make build                      # → bin/tinybouncer (Go ≥ 1.23, stdlib only, no deps)

# 2. Provide the Jev API key (environment only; never in project files)
export TINY_BOUNCER_JEV_API_KEY=…  # or TYPESAFE_API_KEY

# 3. Screen commands by hand
bin/tinybouncer check <<< '{"commands":["git status","rm -rf /"]}'

# 4. Wire the plugin into OpenCode (opencode.jsonc)
```

```jsonc
// opencode.jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [
    { "package": "./opencode/plugins/tiny-bouncer" }
  ]
}
```

The plugin looks for `tinybouncer` on `PATH` by default; set the `executable` option to
point at `bin/tinybouncer` explicitly:

```jsonc
{ "package": "./opencode/plugins/tiny-bouncer",
  "options": { "executable": "/abs/path/to/tiny-bouncer/bin/tinybouncer",
               "onError": "ask", "timeoutMs": 20000 } }
```

### Other backends

Instead of Jev, screen with a local chat model served by LM Studio (or any
OpenAI-compatible endpoint), or with Apple Foundation Models on-device:

```sh
# OpenAI-compatible endpoint (LM Studio serving Gemma-4-E2B)
export TINY_BOUNCER_API_BASE_URL=http://127.0.0.1:1234/v1
export TINY_BOUNCER_API_MODEL=gemma-4-e2b-it-qat@q4_k_xl
bin/tinybouncer check --backend api <<< '{"commands":["git status","rm -rf /"]}'

# macOS 27+ with Apple Intelligence enabled (the `fm` CLI)
bin/tinybouncer check --backend afm <<< '{"commands":["git status","rm -rf /"]}'

# Strands Decider 2B served locally (the same System One battery as Jev)
uv venv --python 3.14 ~/.venvs/decider && uv pip install --python ~/.venvs/decider/bin/python strands-decider
uv run --python ~/.venvs/decider/bin/python strands-decider serve \
  StrandsAgents/strands-decider-2B-hobson-v21 --device mps --port 8000 --strict-window
export TINY_BOUNCER_DECIDER_BASE_URL=http://127.0.0.1:8000
bin/tinybouncer check --backend decider <<< '{"commands":["git status","rm -rf /"]}'
```

The `decider` server is an external Python process installed out of tree; the
repository stays Go and TypeScript. Its thresholds are calibrated for the v21 checkpoint
(`dtv2`; see the comparison below).

Select one for the plugin by setting `TINY_BOUNCER_BACKEND` (e.g. `api`) or the plugin's
`backend` option.

## CLI reference

```sh
tinybouncer check [--backend jev|mock|api|afm|decider] [--cache|--no-cache] < commands.json
tinybouncer eval  --backend jev|api|afm|decider [--compare] [--against <backend>] [--sweep]   # labelled-corpus evaluation
tinybouncer doctor [--backend jev|mock|api|afm|decider]                 # credentials/endpoint health
```

### `check` output contract

```json
{
  "results": [
    { "command": "git status", "verdict": "allow", "confidence": 0.97,
      "categories": ["vcs_read"], "reason": "read-only inspection" }
  ],
  "aggregate": { "effect": "deny", "reason": "…" },
  "meta": { "backend": "jev", "backend_model": "jev-1.13.0",
            "policy_version": "jev-policy-1.0", "thresholds_version": "tv2",
            "wall_ms": 1420, "cached": false, "attempts": "1" }
}
```

Exit codes: `0` success (even when the verdict is a degraded fallback — the JSON is the
contract), `1` usage/config error, `2` internal error. Diagnostics go to stderr.

### `doctor` output

`doctor` prints one **compact, line-delimited JSON object per backend** on stdout
(grouping is by line, not indented), with a human-readable summary on stderr and exit
code `0` only when every reported backend is healthy:

```sh
$ bin/tinybouncer doctor --backend mock   # stdout (compact, one line per backend):
{"backend":"mock","ok":true,"model":"mock-rules","policy_version":"mock-0","thresholds_version":"mock-0","error":""}
# → stderr (human-readable summary): tinybouncer doctor: mock: ok (model mock-rules, policy mock-0, thresholds mock-0)
```

The plugin runs `doctor --backend <selected>` asynchronously at load (best effort,
scoped to the backend it will use) and logs the healthy model/threshold facts, or a
single warning that screening will fall back to `ask`.

### Observed operating point (live, 2026-10-03)

Measured during calibration on the full 265-record corpus, model resolved as
`jev-1.13.0` (requested via alias `jev-latest`), thresholds `tv2`:

- **FNR = 0** (hard gate: no dangerous command auto-allowed), TP 171 / FN 0 / FP 13 /
  TN 81; FPR ≈ 0.138, three-way accuracy ≈ 0.84.
- Latency p50 ≈ 269 ms, **p95 ≈ 450 ms** per request.
- Cost ≈ **972 input tokens per screening request** — roughly **$0.0108 per full
  265-request eval run**, i.e. ~$0.000041 per real permission screening (pricing is
  per input token; output tokens free). Usage is recorded in eval reports.

### Cross-backend comparison (`eval --against`)

`eval --backend <a> --against <b>` runs the corpus through `<a>`, then compares that run
with the most recent stored report for `<b>` (chosen by report timestamp). It prints
metric deltas (current − other), a 3×3 verdict-agreement matrix (rows `<a>`, columns
`<b>`), the disagreeing records (`id`, truth, both verdicts), and a **safety-critical
summary** — commands one side auto-allowed (`allow`) while the other side flagged them
and the truth is `ask`/`deny`. The comparison is written as
`reports/compare-<ts>-<a>-vs-<b>.json`. If no report exists for `<b>`, the command
prints a note and exits 0 (nothing to compare yet).

```sh
# compare the API backend with the recorded Jev run
bin/tinybouncer eval --backend api --against jev
```

Regression gates are per backend when `--compare` or `--against` is in use: the default
gates file is `reports/gates-<backend>.json`, falling back to the shared
`reports/gates.json` (the Jev gates). An explicit `--gates <file>` still wins.

### API/AFM calibration (live, 2026-10-06)

Both chat backends were measured on the full 265-record corpus (cache off: `api` on LM
Studio serving Gemma-4-E2B, `afm` on-device through `fm`). They are **comparison-only**:
neither reaches the hard `FNR = 0` gate, so the production default stays `jev` and no
per-backend gates file is committed.

| backend | model | TP/FN/FP/TN | FNR | FPR | accuracy3 | p50/p95 |
|---|---|---|---|---|---|---|
| `api` | `gemma-4-e2b-it-qat@q4_k_xl` | 131/40/10/84 | 0.234 | 0.106 | 0.608 | 1.60 / 2.17 s |
| `afm` | `system` | 168/3/64/30 | 0.018 | 0.681 | 0.668 | 2.14 / 2.57 s |

`api` auto-allowed 40 dangerous/ask-worthy commands and `afm` 3; both interrupted many
more safe commands than Jev (FPR 0.106 and 0.681). In the committed `--against jev`
comparisons each chat backend auto-allows commands Jev flags (`api` 40, `afm` 3), while
Jev auto-allows none that either flags. `afm` timed out on 3 of 265 requests at its 30 s
budget and failed safe to `ask`. Cost is zero marginal for both; `api` reports token
usage (265 req · 79,115 in · 28,936 out), `afm` none. Because no per-backend gates file
exists, `eval --backend api|afm --compare` applies the shared Jev gates and reports
FAILED by design — the comparison-only decision, not a regression. `decider` is
comparison-only for the same reason (see below).

The committed cross-backend comparison (Jev, `decider`, `api`, `afm` and the mock floor)
is summarised in [`reports/summary-backends-2026-10-07.md`](reports/summary-backends-2026-10-07.md);
the earlier [`reports/summary-backends-2026-10-06.md`](reports/summary-backends-2026-10-06.md)
remains the T16 record for `api`/`afm`.

### Decider (live, 2026-10-07)

The `decider` backend screens commands with a locally served Strands Decider 2B checkpoint
(`StrandsAgents/strands-decider-2B-hobson-v21`), asking the identical hazard battery and
severity question as Jev and routing the answers with the same arithmetic
(`doc/architecture.md` §5quinquies). Calibrated on the 265-record corpus (`dtv2`):

| backend | model | TP/FN/FP/TN | FNR | FPR | accuracy3 | p50/p95 |
|---|---|---|---|---|---|---|
| `decider` | `strands-decider-2B-hobson-v21` | 171/0/39/55 | 0.000 | 0.415 | 0.766 | 2.63 / 3.13 s |

The decider is safe on this corpus (`FNR = 0`, and it auto-allows nothing Jev flags) but it
is **comparison-only**: it interrupts 41.5 % of safe commands against Jev's 13.8 %, is less
accurate three-way, and is roughly eight times slower. No gates file is committed, so
`eval --backend decider --compare` applies the shared Jev gates and reports FAILED by
design.

## Backends

| Backend | Network | Purpose |
|---|---|---|
| `jev` (default) | TypeSafe System One API | Real judgments: hazard probabilities + severity, thresholds in code |
| `api` | OpenAI-compatible endpoint (e.g. LM Studio) | Chat model: one schema-constrained completion per command (Gemma-4-E2B for now) |
| `afm` | on-device (Apple Foundation Models) | Chat model through the `fm` CLI; macOS 27 + Apple Silicon |
| `decider` | local Strands Decider server (e.g. `127.0.0.1:8000`) | Decision model: the same System One battery as Jev, judged locally |
| `mock` | none | Deterministic rules for offline tests and the eval floor |

`api`, `afm` and `decider` are **optional**: `doctor`'s default report omits one whose
endpoint (`TINY_BOUNCER_API_BASE_URL`, `TINY_BOUNCER_DECIDER_BASE_URL`) or CLI (`fm`) is
not configured, so an unused optional backend cannot fail an otherwise-healthy run;
`doctor --backend <id>` still reports it. `api` and `afm` are **comparison-only** (see the
calibration section above): neither meets the hard `FNR = 0` safety gate, so the default
backend stays `jev`. `decider` is comparison-only as well: calibrated to `FNR = 0` on the
corpus, but at FPR 0.415 and p95 ≈ 3 s.

Adding a backend means one package under `internal/backend/<name>` implementing the
three obligations in `doc/architecture.md` §5.2 — no changes to the CLI contract, the
eval harness, or the plugin.

## Plugin options

| Option | Default | Meaning |
|---|---|---|
| `executable` | `tinybouncer` | Binary path (absolute or on `PATH`) |
| `timeoutMs` | `20000` | Kill timer per `check` invocation; must exceed the classifier's own budget |
| `onError` | `ask` | Effect when the classifier fails: `ask` \| `deny` \| `allow` |
| `backend` | *(unset)* | Backend id passed as `--backend`; unset = the CLI's own resolution (`TINY_BOUNCER_BACKEND`, default `jev`) |
| `grantFromAsk` | `false` | Let a classifier `allow` relax a configured `ask` — the only widening path |
| `logDecisions` | `false` | Log verdicts with a hash of the command — never the text |

Notes: non-`shell` permission checks pass through untouched; commands split from
compound shell strings are screened in one batched call; an empty/absent classifier
result is treated the same as any other failure (i.e. `onError`). Strictness is
never loosened by the classifier: a `deny` never softens, an `ask` never becomes an
`allow`, and only `grantFromAsk` — only when the classifier says `allow` — relaxes a
configured `ask`. At plugin load, `tinybouncer doctor` runs asynchronously: a healthy
backend logs its model and thresholds; otherwise a single warning says screening
will fall back to `ask`.

Plugin **logging is best effort**: the hook logs through OpenCode's plugin-log context
(`ctx.log`) when available and falls back to `console.log`/`console.warn` otherwise —
some OpenCode embedded runtimes do not surface console output, so absence of log lines
is not itself a fault. Decision logs (with `logDecisions: true`) contain verdict plus a
sha256 hash of the command batch, never the command text.

### Environment (backends; read by the `tinybouncer` binary, not the plugin)

| Variable | Meaning | Default |
|---|---|---|
| `TINY_BOUNCER_BACKEND` | backend id when no `--backend`/option is set | `jev` |
| `TINY_BOUNCER_JEV_API_KEY` / `TYPESAFE_API_KEY` | Jev credentials | — |
| `TINY_BOUNCER_JEV_BASE_URL` / `TYPESAFE_ENDPOINT` | Jev endpoint override | `https://api.typesafe.ai` |
| `TINY_BOUNCER_JEV_MODEL` | Jev model or alias | `jev-latest` |
| `TINY_BOUNCER_TIMEOUT_MS` | per-request timeout inside the classifier | `15000` |
| `TINY_BOUNCER_RETRIES` | transport retry attempts | `3` |
| `TINY_BOUNCER_CACHE` | response cache on/off (`check` only) | on |
| `TINY_BOUNCER_API_BASE_URL` | OpenAI-compatible endpoint (`api` backend) | required for `api` |
| `TINY_BOUNCER_API_MODEL` | model id (`api` backend) | `gemma-4-e2b-it-qat@q4_k_xl` |
| `TINY_BOUNCER_API_KEY` | bearer token (`api` backend), if any | — |
| `TINY_BOUNCER_AFM_EXECUTABLE` | `fm` CLI path (`afm` backend) | `fm` |
| `TINY_BOUNCER_CHAT_CONCURRENCY` | chat backend fan-out width | `1` |
| `TINY_BOUNCER_CHAT_TIMEOUT_MS` | per-command chat timeout (ms) | `30000` |
| `TINY_BOUNCER_DECIDER_BASE_URL` | Strands Decider server (`decider` backend) | required for `decider` |
| `TINY_BOUNCER_DECIDER_MODEL` | model alias sent to the decider | `strands-decider-latest` |
| `TINY_BOUNCER_DECIDER_TIMEOUT_MS` | per-request decider timeout (ms) | `30000` |
| `TINY_BOUNCER_DECIDER_RETRIES` | decider transport retry attempts | `3` |
| `TINY_BOUNCER_DECIDER_CONCURRENCY` | decider fan-out width | `1` |
| `TINY_BOUNCER_DECIDER_THRESHOLDS` | decider threshold sweep override | — |

### Manual TUI verification checklist

Run a real OpenCode session with the plugin registered (see the snippets above) and
confirm each step:

1. **Startup** — the OpenCode log shows the plugin's setup lines: classifier facts
   (`tinybouncer: backend …: model …, policy …, thresholds …`) or the single warning
   `tinybouncer: screening will fall back to ask`; the session start is never delayed.
2. **Allow without prompt** — ask for a read-only command (e.g. `git status`; with the
   `mock` backend or benign Jev probabilities) and confirm it runs without an
   approval prompt.
3. **Classified `ask` → prompt with reason** — run a borderline command (mock:
   `git reset --hard`; Jev: one that lands in the ask band) and confirm the
   interactive approval prompt appears and carries the classifier reason.
4. **Deny is final** — run a dangerous command (e.g. `rm -rf /`; mock rules deny it
   with high confidence) and confirm the command is rejected with the classifier's
   denial message, and no prompt offers to run it.
5. **Outage → interactive ask** — stop the classifier (rename `bin/tinybouncer`, or point
   `executable` at a missing path) and run any command: the `onError` effect (default
   `ask`) shows the normal interactive approval prompt, and the outage is named in
   the permission message.
6. **Privacy** — with `logDecisions: true`, check the plugin log lines contain a
   64-hex command hash and verdicts only; grep the log for the raw command text to
   confirm it is absent.

## Security and privacy

- API keys come from the environment only; they are never written to project files or
  logs.
- Raw command text is sent to the judgment backend (it must, to judge) but never
  persisted on disk: the response cache stores hashes and verdicts only; plugin logs
  record a command hash, not the text.
- TypeSafe documents Jev as not trained on customer requests — check their DPA/ZDR
  posture when wiring production keys (see `doc/architecture.md` §9).
- With `api`, `afm` and `decider`, commands normally stay on the machine: `afm` runs
  entirely on-device, the `api` endpoint is typically a local server (LM Studio, Ollama),
  and the decider server is a local process. A remote API endpoint carries the same "raw
  command text is sent to the judge" caveat as Jev.

## Troubleshooting

- **`401 Unauthorized` in `doctor`** — key missing or wrong. Check
  `TINY_BOUNCER_JEV_API_KEY` / `TYPESAFE_API_KEY` (and `TINY_BOUNCER_JEV_BASE_URL` if you run
  a proxy).
- **Agent execution feels slow** — the first call warms the model round trip; every
  command is one Jev request. Use a cache (`--cache`, default on) and keep the timeout
  (`TINY_BOUNCER_TIMEOUT_MS`) reasonable; verify with `bin/tinybouncer eval --bench-spawn`
  and the `wall_ms` p95 in `reports/`.
- **Classifier outage mid-session** — the plugin applies `onError` (default `ask`), so
  you should see interactive prompts; `tinybouncer doctor` tells you what's wrong.
- **Cache weirdness after tuning** — keys include backend, model, policy and thresholds
  versions; tuning thresholds invalidates old entries automatically.

## For developers

- `doc/architecture.md` — behaviour authority (contracts, battery, thresholds, gates).
- `doc/plan.md` — task queue + session protocol (task sessions implement one task,
  verify acceptance criteria, commit).
- `AGENTS.md` — how agents are expected to work in this repo.
- `make ci` — the full offline verification target (`fmt`, `vet`, `build`, `test`,
  `eval-mock`, `doctor --backend mock`); `.github/workflows/ci.yml` runs the same set
  on every push (ubuntu, Go 1.25, no secrets required).
- `make eval-live` — opt-in live run against Jev (`eval --backend jev --compare`); it
  applies the regression gates from `reports/gates.json` (`FNR = 0` hard, FPR ≤ 0.15,
  accuracy3 ≥ 0.80, p95 ≤ 1200 ms). With no key set it prints a warning and skips
  safely. Every run appends one row to `reports/history.jsonl` — the committed
  performance record (kept intentionally, including the calibration trail); reports
  contain synthetic corpus commands only.
- `make eval-api` / `make eval-afm` — opt-in live runs against the chat backends
  (`eval --backend api|afm --compare`). `eval-api` needs `TINY_BOUNCER_API_BASE_URL`
  (LM Studio serving Gemma-4-E2B by default); `eval-afm` needs a ready on-device
  model (`fm available`). Either prints a warning and skips safely when unavailable.
  Both backends are comparison-only (no per-backend gates file), so these targets
  apply the shared Jev gates and report FAILED by design.
- `make eval-decider` — opt-in live run against a locally served Strands Decider
  checkpoint (`eval --backend decider --compare`); needs
  `TINY_BOUNCER_DECIDER_BASE_URL` and skips safely without it.
- Plugin: `cd opencode/plugins/tiny-bouncer && npm run typecheck && npm run test`.
