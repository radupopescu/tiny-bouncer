# Wise Yolo

Wise Yolo screens shell commands requested by LLM agents before they run. It sends each
command to an external judgment backend — TypeSafe's **Jev** by default — and turns the
verdict into a permission decision in the [OpenCode V2](https://opencode.ai/v2/docs/)
harness: allow the command, block it, or fall back to the normal interactive prompt.

> **Status: v0.1.0.** All planned tasks are complete (see `doc/plan.md`); the live
> operating point is calibrated and gated (`FNR = 0` hard). Manual user smoke test
> pending under way of working notes.

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
permission.evaluate hook ──▶ wiseyolo check ──▶ Jev judgment (probabilities)
                             (one process per   ▲         │
                              permission event) └─ thresholds (yours, in code)
```

- The OpenCode plugin intercepts shell permission checks, batches the commands it was
  given, and spawns `wiseyolo check` once per event.
- `wiseyolo` normalises, optionally caches, calls the selected backend, and applies
  your thresholds to convert probabilities into an effect.
- The plugin maps that effect onto the OpenCode permission decision: a classifier
  `deny` blocks with a reason, an `ask` surfaces the interactive prompt, and an `allow`
  leaves your configured permission decision untouched.

## Quickstart

```sh
# 1. Build the classifier
make build                      # → bin/wiseyolo (Go ≥ 1.23, stdlib only, no deps)

# 2. Provide the Jev API key (environment only; never in project files)
export WISE_YOLO_JEV_API_KEY=…  # or TYPESAFE_API_KEY

# 3. Screen commands by hand
bin/wiseyolo check <<< '{"commands":["git status","rm -rf /"]}'

# 4. Wire the plugin into OpenCode (opencode.jsonc)
```

```jsonc
// opencode.jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [
    { "package": "./opencode/plugins/wise-yolo" }
  ]
}
```

The plugin looks for `wiseyolo` on `PATH` by default; set the `executable` option to
point at `bin/wiseyolo` explicitly:

```jsonc
{ "package": "./opencode/plugins/wise-yolo",
  "options": { "executable": "/abs/path/to/wise-yolo/bin/wiseyolo",
               "onError": "ask", "timeoutMs": 20000 } }
```

## CLI reference

```sh
wiseyolo check [--backend jev|mock] [--cache|--no-cache] < commands.json
wiseyolo eval  --backend jev [--compare] [--sweep]   # labelled-corpus evaluation
wiseyolo doctor [--backend jev]                       # credentials/endpoint health
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
$ bin/wiseyolo doctor --backend mock   # stdout (compact, one line per backend):
{"backend":"mock","ok":true,"model":"mock-rules","policy_version":"mock-0","thresholds_version":"mock-0","error":""}
# → stderr (human-readable summary): wiseyolo doctor: mock: ok (model mock-rules, policy mock-0, thresholds mock-0)
```

The plugin runs `doctor` asynchronously at load (best effort) and logs the healthy
model/threshold facts, or a single warning that screening will fall back to `ask`.

### Observed operating point (live, 2026-10-03)

Measured during calibration on the full 265-record corpus, model resolved as
`jev-1.13.0` (requested via alias `jev-latest`), thresholds `tv2`:

- **FNR = 0** (hard gate: no dangerous command auto-allowed), TP 171 / FN 0 / FP 13 /
  TN 81; FPR ≈ 0.138, three-way accuracy ≈ 0.84.
- Latency p50 ≈ 269 ms, **p95 ≈ 450 ms** per request.
- Cost ≈ **972 input tokens per screening request** — roughly **$0.0108 per full
  265-request eval run**, i.e. ~$0.000041 per real permission screening (pricing is
  per input token; output tokens free). Usage is recorded in eval reports.

## Backends

| Backend | Network | Purpose |
|---|---|---|
| `jev` (default) | TypeSafe System One API | Real judgments: hazard probabilities + severity, thresholds in code |
| `mock` | none | Deterministic rules for offline tests and the eval floor |

Adding a backend means one package under `internal/backend/<name>` implementing the
three obligations in `doc/architecture.md` §5.2 — no changes to the CLI contract, the
eval harness, or the plugin.

## Plugin options

| Option | Default | Meaning |
|---|---|---|
| `executable` | `wiseyolo` | Binary path (absolute or on `PATH`) |
| `timeoutMs` | `20000` | Kill timer per `check` invocation; must exceed the classifier's own budget |
| `onError` | `ask` | Effect when the classifier fails: `ask` \| `deny` \| `allow` |
| `backend` | *(unset)* | Backend id passed as `--backend`; unset = the CLI's own resolution (`WISE_YOLO_BACKEND`, default `jev`) |
| `grantFromAsk` | `false` | Let a classifier `allow` relax a configured `ask` — the only widening path |
| `logDecisions` | `false` | Log verdicts with a hash of the command — never the text |

Notes: non-`shell` permission checks pass through untouched; commands split from
compound shell strings are screened in one batched call; an empty/absent classifier
result is treated the same as any other failure (i.e. `onError`). Strictness is
never loosened by the classifier: a `deny` never softens, an `ask` never becomes an
`allow`, and only `grantFromAsk` — only when the classifier says `allow` — relaxes a
configured `ask`. At plugin load, `wiseyolo doctor` runs asynchronously: a healthy
backend logs its model and thresholds; otherwise a single warning says screening
will fall back to `ask`.

Plugin **logging is best effort**: the hook logs through OpenCode's plugin-log context
(`ctx.log`) when available and falls back to `console.log`/`console.warn` otherwise —
some OpenCode embedded runtimes do not surface console output, so absence of log lines
is not itself a fault. Decision logs (with `logDecisions: true`) contain verdict plus a
sha256 hash of the command batch, never the command text.

### Environment (backends; read by the `wiseyolo` binary, not the plugin)

| Variable | Meaning | Default |
|---|---|---|
| `WISE_YOLO_BACKEND` | backend id when no `--backend`/option is set | `jev` |
| `WISE_YOLO_JEV_API_KEY` / `TYPESAFE_API_KEY` | Jev credentials | — |
| `WISE_YOLO_JEV_BASE_URL` / `TYPESAFE_ENDPOINT` | Jev endpoint override | `https://api.typesafe.ai` |
| `WISE_YOLO_JEV_MODEL` | Jev model or alias | `jev-latest` |
| `WISE_YOLO_TIMEOUT_MS` | per-request timeout inside the classifier | `15000` |
| `WISE_YOLO_RETRIES` | transport retry attempts | `3` |
| `WISE_YOLO_CACHE` | response cache on/off (`check` only) | on |

### Manual TUI verification checklist

Run a real OpenCode session with the plugin registered (see the snippets above) and
confirm each step:

1. **Startup** — the OpenCode log shows the plugin's setup lines: classifier facts
   (`wiseyolo: backend …: model …, policy …, thresholds …`) or the single warning
   `wiseyolo: screening will fall back to ask`; the session start is never delayed.
2. **Allow without prompt** — ask for a read-only command (e.g. `git status`; with the
   `mock` backend or benign Jev probabilities) and confirm it runs without an
   approval prompt.
3. **Classified `ask` → prompt with reason** — run a borderline command (mock:
   `git reset --hard`; Jev: one that lands in the ask band) and confirm the
   interactive approval prompt appears and carries the classifier reason.
4. **Deny is final** — run a dangerous command (e.g. `rm -rf /`; mock rules deny it
   with high confidence) and confirm the command is rejected with the classifier's
   denial message, and no prompt offers to run it.
5. **Outage → interactive ask** — stop the classifier (rename `bin/wiseyolo`, or point
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

## Troubleshooting

- **`401 Unauthorized` in `doctor`** — key missing or wrong. Check
  `WISE_YOLO_JEV_API_KEY` / `TYPESAFE_API_KEY` (and `WISE_YOLO_JEV_BASE_URL` if you run
  a proxy).
- **Agent execution feels slow** — the first call warms the model round trip; every
  command is one Jev request. Use a cache (`--cache`, default on) and keep the timeout
  (`WISE_YOLO_TIMEOUT_MS`) reasonable; verify with `bin/wiseyolo eval --bench-spawn`
  and the `wall_ms` p95 in `reports/`.
- **Classifier outage mid-session** — the plugin applies `onError` (default `ask`), so
  you should see interactive prompts; `wiseyolo doctor` tells you what's wrong.
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
- Plugin: `cd opencode/plugins/wise-yolo && npm run typecheck && npm run test`.
