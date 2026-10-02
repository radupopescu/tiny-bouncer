# Wise Yolo

Wise Yolo screens shell commands requested by LLM agents before they run, using an
external judgment backend (TypeSafe's Jev by default), and integrates with the
OpenCode V2 harness via a `permission.evaluate` plugin hook. Fail-safe to `ask`.

See `doc/architecture.md` (behaviour) and `doc/plan.md` (implementation plan).

```sh
make build   # → bin/wiseyolo
wiseyolo check --backend jev <<< '{"commands":["git status"]}'
```
