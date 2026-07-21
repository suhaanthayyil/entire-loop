# entire-loop

A self-prompting agent-org **graph loop**, packaged as an Entire CLI plugin.

Where an agent is one worker on one prompt, `entire-loop` treats a goal as a
small **organization of seats** that runs as a **loop**: plan the round, fan the
seats out concurrently, verify, synthesize, and repeat until the goal is met or
the round budget is spent. Loops beat single shots because each round is briefed
from the last — the org's own structural graph and durable brain feed the next
plan.

It is a **plugin**, so it composes with the rest of Entire's code intelligence:
each worker seat is briefed from the sibling `graph` (structural symbols) and
`brain` (durable knowledge) plugins, with **hybrid per-seat wiring** — cheap
seats read a pre-built brief with MCP off, deep seats bind the brain MCP server
directly.

> **Workers are non-mutating.** Every seat runs in Claude Code **plan mode**: it
> reads, analyzes, and **PROPOSES** (the build seat emits a proposed unified
> diff as text). Nothing is written to the target repo.

## What a round does

```
goal ─▶ plan (research, build, critic, measure)
          │
          ▼  fan out concurrently (bounded by --jobs)
     ┌────────────┬───────────┬───────────┬───────────┐
   research     build       critic      measure
  (brain MCP) (brief only) (brain MCP) (brief only)
     └────────────┴───────────┴───────────┴───────────┘
          │
          ▼  verify + synthesize
     metrics + verdict + goal-met  ──▶  merge into run state  ──▶  next round
```

- **research** *(deep — brain MCP on)* — maps the code, facts, and risks for the goal.
- **build** *(cheap — brief only)* — proposes the smallest change as a unified diff.
- **critic** *(deep — brain MCP on)* — verifies the work and sets the goal-met flag.
- **measure** *(cheap — brief only)* — turns the round into numeric progress/risk metrics.

The plan is fixed in this MVP behind a `Planner` interface; a Phase-B control
seat (see `internal/templates/templates/control.md`) can emit the plan as JSON.
A no-op `Reorg` seam is wired in for runtime reorganization (small→solo,
fail-cluster→+critic, budget>progress→collapse, 2×fix→promote).

## Install

Requires the parent `entire` CLI plus the `graph` and `brain` plugins.

```sh
sh scripts/install.sh          # verifies siblings, then builds + registers
# or just the plugin:
sh scripts/install-local.sh
```

## Usage

```sh
entire loop "make the flaky ingest test deterministic"
entire loop run "add a --dry-run flag to the importer" --rounds 2 --jobs 3
entire loop status            # recent runs + their state
entire loop doctor            # env + sibling (graph/brain) reachability
entire loop version --json
```

`entire loop "<goal>"` is shorthand for `entire loop run "<goal>"`.

### Flags (`run`)

| Flag | Default | Meaning |
|---|---|---|
| `--repo` | `ENTIRE_REPO_ROOT` → git discovery | Repository root the loop reasons over |
| `--rounds` | `1` | Maximum loop rounds |
| `--jobs` | `2` | Max concurrent worker seats (the concurrency cap) |
| `--model` | *(agent default)* | Worker model override (`claude --model`) |
| `--effort` | *(agent default)* | Worker reasoning effort (`low`/`medium`/`high`) |

Run state is persisted to `ENTIRE_PLUGIN_DATA_DIR/runs/<runID>/state.json`
(falling back to `~/.local/share/entire/plugins/data/loop` when the env var is
unset), rewritten after every round.

## No-egress

`entire-loop` honors the brain's local-only switch. When
`ENTIRE_BRAIN_NO_EGRESS` or `ENTIRE_BRAIN_LOCAL_ONLY` is set, the MVP worker
(Claude Code) is **refused** — it can send selected brain context outside the
local loopback. Only local-loopback backends are permitted under no-egress; a
local worker path is left for a future revision.

## Reliability

- **Process-group reaping.** Each worker runs in its own process group; on
  timeout or cancellation the whole group is signaled (SIGTERM, then SIGKILL
  after a grace) so `node`/`claude` grandchildren never leak.
- **Per-worker wall-clock timeout** (default 20 min).
- **Graceful degradation.** A garbled or missing worker envelope, or a
  sibling being down, degrades the round with a warning — it never aborts.
- **Idempotent skip.** A completed seat's result is cached in the run dir and
  reused on re-run.

## License

MIT — see [LICENSE](LICENSE).
