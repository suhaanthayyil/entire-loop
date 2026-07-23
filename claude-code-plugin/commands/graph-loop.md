---
description: Run the entire-loop self-planning agent-org graph loop over a goal via `entire loop`.
argument-hint: <goal> [--planner llm|fixed] [--plan-mode dynamic|immutable] [--rounds N] [--max-rounds N] [--jobs K] [--allow-mutating-build] [--measure-cmd "<cmd>"]
allowed-tools: [Bash]
---

Run the graph loop over the goal in `$ARGUMENTS` by shelling the entire-loop Go
core. Do not re-implement any orchestration — the core does the planning, seat
fan-out, verification, and convergence.

Prerequisite: the `entire` CLI and the `graph` plugin must be on PATH (the loop
runs as `entire loop`); the `brain` plugin is optional. If `$ARGUMENTS` is empty,
ask the user for a goal instead of running.

Run this with the Bash tool (quote the goal exactly as given):

```sh
entire loop "$ARGUMENTS"
```

`entire loop "<goal>"` is shorthand for `entire loop run "<goal>"`. If the user's
text includes flags, pass them through — valid flags: `--planner llm|fixed`,
`--plan-mode dynamic|immutable`, `--rounds N`, `--max-rounds N`, `--jobs K`,
`--measure-cmd "<cmd>"` (read-only), `--allow-mutating-build`, `--repo <path>`,
`--model`, `--effort`. By default the loop converges (loop-until-dry) and every
seat runs in plan mode (proposes only). Only add `--allow-mutating-build` when the
user explicitly asks the loop to WRITE code — it spawns a bypassPermissions worker
in an isolated throwaway clone with no OS sandbox; never use it on untrusted
goals/repos.

After it finishes, report the outcome and point the user at `entire loop status`
(reads back run state from `$ENTIRE_PLUGIN_DATA_DIR/runs/<id>/state.json`). A run
refused under no-egress mode exits non-zero — surface that rather than retrying.
