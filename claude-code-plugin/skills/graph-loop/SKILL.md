---
name: graph-loop
description: >
  Run the entire-loop self-planning agent-org GRAPH LOOP over a goal. Use when a
  task is bigger than one prompt — "loop on this until done", "converge on X",
  "fan out agents to fix/build/harden Y", "plan → build → verify → measure over
  rounds". Treats a goal as an organization of worker seats that run as a loop:
  an LLM control plane plans each round, seats (research → build → critic →
  measure/synthesize) fan out, the round is verified and synthesized, and the
  plan is rewritten from the result — round after round until the goal is met or
  the round budget is spent. Shells the entire-loop Go core via `entire loop`.
  Trigger: "graph loop", "entire loop", "loop until done", "converge on goal",
  "agent org over a goal", "run rounds until the goal is met".
---

Graph-loop is a thin Claude Code shim over ONE core: the `entire-loop` Go binary,
invoked as `entire loop`. This skill does not re-implement any orchestration — it
tells you WHEN to reach for the loop and HOW to shell it. All planning, seat
fan-out, verification, and convergence live in the core.

## Loops → graphs framing

A single agent is one worker on one prompt. The graph loop treats a goal as a
small **organization of seats** that runs as a **loop**. Each **round** is a
graph (a DAG of typed nodes/edges), not a flat fan-out: data flows across edges
so a downstream seat consumes its upstream's validated output —
`research → build → critic → router → verifier → measure → synthesize → round
result`. The control plane then re-plans the next round from that result. Loops
beat single shots because each round is briefed from the last, and the org's own
structural **graph** (symbols/edges, required) plus durable **brain** (optional)
feed the next plan. By default it **converges** (loop-until-dry) rather than
running a fixed count.

## When to use

- The task is bigger than one prompt: multi-file change, harden/refactor/make-pass.
- You want iterative convergence with verification, not a single best-effort shot.
- You want the change proposed-and-verified (default) or actually written in an
  isolated clone (opt-in `--allow-mutating-build`).

Do NOT use for a one-line answer, a single obvious edit, or pure Q&A — just answer.

## How to run

Shell the core with the Bash tool. `entire loop "<goal>"` is shorthand for
`entire loop run "<goal>"`.

```sh
entire loop "make the flaky ingest test deterministic"
entire loop run "add a --dry-run flag to the importer" --rounds 2 --jobs 3
entire loop status            # recent runs + their state
entire loop doctor            # env + graph/brain reachability
entire loop version --json
```

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--planner llm\|fixed` | `llm` | HOW a round is planned: self-planning LLM control plane vs static roster |
| `--plan-mode dynamic\|immutable` | `dynamic` | WHEN the DAG is planned: re-plan every round vs plan once up front |
| `--rounds N` | `0` | Fixed round count (`0` = converge until dry); also caps max-rounds |
| `--max-rounds N` | `6` | Safety cap on rounds in converge mode |
| `--jobs K` | `2` | Max concurrent worker seats |
| `--measure-cmd "<cmd>"` | *(off)* | Read-only command; its JSON stdout (e.g. `{"progress":0.8}`) overrides model-derived round metrics so the loop converges on a real signal |
| `--allow-mutating-build` | `false` | Let the build seat write real code in an isolated throwaway clone (see security) |
| `--repo <path>` | `ENTIRE_REPO_ROOT` → git discovery | Repo root the loop reasons over |
| `--model` / `--effort` | *(agent default)* | Worker model / reasoning-effort override |

### Prerequisites

- `entire` (parent Entire CLI) on PATH — the plugin runs as `entire loop`.
- `entire-graph` (the `graph` plugin) — **required** structural provider.
- `entire-brain` (the `brain` plugin) — **optional**, auto-used when installed,
  degrades to graph + goal otherwise. This plugin's `.mcp.json` declares
  `entire-brain` so deep seats can bind it when present.
- Run `entire loop doctor` first to confirm env + sibling reachability.

## Security note on `--allow-mutating-build`

**OFF by default.** Without it, every seat runs in Claude Code **plan mode** and
only reads, analyzes, and **proposes** — the target repo is never edited. When you
pass `--allow-mutating-build`, the build seat spawns a `claude` worker in
**`bypassPermissions`** mode: it can write files, run shell, and reach the
network. It is confined to a **throwaway `git clone --local --no-hardlinks`** (own
object store/refs; `origin` removed; env scrubbed to a minimal allowlist; clone
discarded on every path), so your working tree/refs/tags are never touched — but
**there is no OS sandbox**, so a hostile worker could still reach the network or
absolute paths outside the clone. **Do not point it at untrusted goals or
repositories.** The LLM planner **cannot** enable the mutating seat — it is
derived solely from the human flag AND `role == build` (privilege lock).
