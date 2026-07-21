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

> **The build seat mutates real code — in isolation.** It runs in Claude Code
> **bypassPermissions** mode inside a *throwaway git worktree* of the target
> repo, so it actually writes the change; the loop captures the worktree's `git
> diff` as its proposal and discards the worktree. The user's real working tree
> is never touched. If the target isn't a git repo, the build seat degrades to
> the old non-mutating propose-as-text path. **research / critic / measure /
> verifiers stay plan-mode read-only.**

## What a round does

A round is a **pipeline** (a DAG), not a flat fan-out: data flows across each
edge, so a downstream seat consumes its upstream's validated output.

```
goal ─▶ plan → reorg
          │
          ▼
     research ──▶ build ──▶ critic ──▶ measure ─▶ synthesize
    (brain MCP)  (worktree) (brain MCP) (metrics)
                    │            ▲
                    ▼            │
                 router ── large change ──▶ verifier-on-edge (3 skeptics,
                    │                          correctness / security / reproduce;
                 small change                  accept only if ≥2 survive)
                    │
                    ▼
        metrics + verdict + goal-met  ──▶  merge into state  ──▶  next round
```

- **research** *(deep — brain MCP on)* — maps the code, facts, and risks.
- **build** *(mutating — throwaway worktree)* — implements the change; its brief
  carries research's findings; the loop captures its diff.
- **router** — inspects the proposal's diff size: a small change gets one quick
  critic pass; a large change triggers the full verifier audit.
- **critic** *(deep — brain MCP on)* — verifies the build proposal; sets goal-met.
- **verifier-on-edge** — before a large proposal is accepted, 3 independent
  skeptics each try to *refute* it through a different lens; it is kept only if a
  majority survive.
- **measure** *(cheap)* — turns the round into numeric progress/risk metrics.

The plan is fixed behind a `Planner` interface; a Phase-B control seat (see
`internal/templates/templates/control.md`) can emit the plan as JSON. A
`RulesReorg` seam adjusts the roster between rounds (small→solo collapses a
one-line goal to build+critic; budget>progress→collapse sheds the expensive
seats).

## Loop-until-dry

By default the loop **converges** rather than running a fixed count: it keeps
running rounds until the goal is met, until `K` consecutive rounds surface no new
finding/proposal (deduped against everything ever *seen*, so a rejected item that
reappears does not count as progress), or until the `--max-rounds` safety cap.
Pass `--rounds N` for fixed-count mode.

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
| `--rounds` | `0` | Fixed round count (`0` = converge until dry); also caps `--max-rounds` |
| `--max-rounds` | `6` | Safety cap on rounds in converge mode |
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
  After SIGKILL a **bounded reap grace** applies: a child stuck in
  uninterruptible sleep can never hang the round — `cmd.Wait` is abandoned to a
  detached goroutine and the seat returns with a "could not be reaped" warning.
- **Per-worker wall-clock timeout** (default 20 min).
- **Graceful degradation.** A garbled or missing worker envelope, a sibling
  being down, a seat panic, or a truncated worker output degrades the round with
  a warning — it never aborts. A round entirely refused under no-egress exits
  non-zero.
- **Isolated mutation.** The build seat writes code only inside a per-round
  throwaway worktree; the target's working tree is never modified.
- **Round-scoped idempotent skip.** A completed *OK* seat result is cached under
  the run dir keyed by round, so a re-run reuses it but round N never replays
  round N-1. Failed results are never cached.

## License

MIT — see [LICENSE](LICENSE).
