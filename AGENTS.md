# AGENTS.md — Way of working (Tiny Bouncer)

Instructions for any coding agent working in this repository: subagent task sessions,
separate OpenCode sessions, and interactive assistants alike.

## What this project is

Tiny Bouncer screens shell commands requested by LLM agents before they run, by calling an
external judgment backend (TypeSafe's Jev by default), and integrates with OpenCode V2
through a `permission.evaluate` plugin hook. It fails safe to `ask`.

## Document map (authority order)

| Document | Role |
|---|---|
| `doc/architecture.md` | **Behaviour authority.** All contracts, policies, thresholds. If code and this doc disagree, the doc wins or a task explicitly amends the doc. |
| `doc/plan.md` | Task queue and session protocol. §4 is the single source of truth for status. |
| `AGENTS.md` | This file. How agents are expected to work here. |

## Session protocol (summary)

The full protocol lives in `doc/plan.md` §1; the essentials:

1. Read `doc/architecture.md` and the current §4 queue before doing anything. Trust the
   repo state, not memory from other sessions.
2. Claim the first task whose status is `pending` and whose dependencies are all `done`:
   set your queue row to `in-progress`, commit, implement, mark `done`, commit.
3. **One task per session.** Never start a second task. Only work on files listed in
   your task spec plus your own row in `doc/plan.md`.
4. Never commit a failing state. Never push unless explicitly asked.
5. If you find a genuine conflict with the architecture, stop and record it in your
   queue row as `blocked` with an explanation — do not silently reinterpret the spec.

## Hard project conventions

- **Go only where Go is expected** (`tinybouncer` binary, `internal/…`); TypeScript only in
  `opencode/plugins/tiny-bouncer/`. No other languages.
- **Zero third-party dependencies.** Go: stdlib only, pinned `go 1.23` in `go.mod`
  (toolchain 1.27.x is fine to compile with). Plugin: `@opencode/plugin` types and the
  Node stdlib only.
- **Every change must keep green**: `go build ./... && go test ./... && go vet ./...`
  plus `test -z "$(gofmt -l .)"`. The Makefile encodes this; use it.
- **Tests never touch the network.** Live/backends are only exercised in explicitly
  gated live runs (`eval` with a key); everything else uses `httptest` or the mock
  backend.
- **Never persist raw command text** (it may contain secrets): cache keys are hashes,
  logs record hashes, eval reports contain only synthetic corpus commands.
- **Commit messages** carry the task id: `Txx: <one-line summary>`; claim commits are
  `Txx: claim (session <name>)`. No `--no-verify`, no scope creep inside a commit.
- **English spelling: British** in docs, comments, and user-facing strings
  (normalise, colour, behaviour…), while keeping code identifiers conventional.

## When doing work outside the task queue

Small fixes and docs updates (like README or AGENTS.md maintenance) do not need a full
 task: keep them in their own commit, name what they are
 (`docs: …`, `make: …`), and keep the §4 queue untouched unless the change is itself a
 task completion. Structural or behavioural changes, however, belong to a task (or a
 new task row) — do not implement them ad hoc.
