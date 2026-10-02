# Wise Yolo

Wise Yolo screens shell commands requested by LLM agents before they run. It sends each
command to an external judgment backend — TypeSafe's **Jev** by default — and turns the
verdict into a permission decision in the [OpenCode V2](https://opencode.ai/v2/docs/)
harness: allow the command, block it, or fall back to the normal interactive prompt.

> **Status: under active development.** The implementation is tracked in
> `doc/plan.md`; see [What works today](#what-works-today).

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
            "policy_version": "jev-policy-1.0", "thresholds_version": "tv1",
            "wall_ms": 1420, "cached": false, "attempts": "1" }
}
```

Exit codes: `0` success (even when the verdict is a degraded fallback — the JSON is the
contract), `1` usage/config error, `2` internal error. Diagnostics go to stderr.

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
| `timeoutMs` | `20000` | Kill timer per `check` invocation |
| `onError` | `ask` | Effect when the classifier fails: `ask` \| `deny` \| `allow` |
| `grantFromAsk` | `false` | Let a confident classifier `allow` relax a configured `ask` |
| `logDecisions` | `false` | Log verdicts with a hash of the command — never the text |

Notes: non-`shell` permission checks pass through untouched; commands split from
compound shell strings are screened in one batched call; an empty/absent classifier
result is treated the same as any other failure (i.e. `onError`).

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
- `make ci` — the full offline verification target
  (`fmt`, `vet`, `build`, `test`, `eval-mock`, doctor).
