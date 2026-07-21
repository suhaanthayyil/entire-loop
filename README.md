# entire-loop

A self-planning agent-org **graph loop**, packaged as an Entire CLI plugin.

Where a single agent is one worker on one prompt, `entire-loop` treats a goal as
a small **organization of seats** that runs as a **loop**: an LLM control plane
plans the round, worker seats (research → build → critic → measure/synthesize)
fan out concurrently, the round is verified and synthesized, and the plan is
rewritten from the result — round after round until the goal is met or the round
budget is spent. Loops beat single shots because each round is briefed from the
last, and the org's own structural **graph** and durable **brain** feed the next
plan.

It is a **plugin**, so it composes with the rest of Entire's code intelligence.
Every worker seat is briefed from the sibling `graph` (structural symbols/edges)
and `brain` (durable knowledge) plugins, with **hybrid per-seat wiring**: cheap
seats read a pre-built brief with MCP off; deep seats bind the brain MCP server
directly.

```
entire loop "<goal>"
        │
        ▼
  ┌───────────────┐     re-plans every round from the last round's result
  │ control plane │◀───────────────────────────────────────────────┐
  │  (LLM planner)│                                                 │
  └───────┬───────┘                                                 │
          ▼                                                         │
   research ──▶ build ──▶ critic ──▶ measure ─▶ synthesize ──▶ round result
  (brain MCP)  (clone)  (brain MCP) (metrics)                       │
                 │           ▲                                      │
                 ▼           │                                      │
              router ── large change ──▶ verifier-on-edge          │
                 │              (3 skeptics: correctness/security/  │
              small change      reproduce; accept only if ≥2 hold) │
                 └──────────────────────────────────────────────────┘
                        merge into state ──▶ next round (converge until dry)
```

## What a round does

A round is a **pipeline** (a DAG), not a flat fan-out: data flows across each
edge, so a downstream seat consumes its upstream's validated output.

- **control plane** *(LLM planner, default)* — plans and re-plans the round's
  seat roster from the accumulated state, refines the goal, and sets sub-goals.
  It is always non-mutating and privilege-locked (see Security).
- **research** *(deep — brain MCP on)* — maps the code, facts, and risks.
- **build** *(brief-only)* — implements the change; its brief carries research's
  findings. By default it **proposes as text**; with `--allow-mutating-build` it
  writes real code in an isolated throwaway clone and the loop captures its diff.
- **router** — inspects the proposal's diff size: a small change gets one quick
  critic pass; a large change triggers the full verifier audit.
- **critic** *(deep — brain MCP on)* — verifies the build proposal; sets goal-met.
- **verifier-on-edge** — before a large proposal is accepted, 3 independent
  skeptics each try to *refute* it through a different lens (correctness,
  security, reproduces); it is kept only if a majority survive.
- **measure** *(cheap)* — turns the round into numeric progress/risk metrics.

### Loop-until-dry

By default the loop **converges** rather than running a fixed count: it keeps
running rounds until the goal is met, until `K` consecutive rounds surface no new
finding/proposal (deduped against everything ever *seen*, so a rejected item that
reappears does not count as progress — `K` = 2), or until the `--max-rounds`
safety cap (default 6). Pass `--rounds N` for fixed-count mode.

## Prerequisites

`entire-loop` is a plugin on top of the Entire CLI and its two sibling
code-intelligence plugins. You need, on your `PATH`:

- **`entire`** — the parent [Entire CLI](https://github.com/entireio) (`entire`
  must resolve; the plugin runs as `entire loop`).
- **`entire-graph`** — the structural provider (symbols + edges), installed as
  the `graph` plugin. Source: <https://github.com/entireio/entire-graph>.
- **`entire-brain`** — the durable knowledge store + MCP tools, installed as the
  `brain` plugin. Source: <https://github.com/entireio/entire-brain>.
- **A logged-in agent CLI** — `claude` ([Claude Code](https://www.anthropic.com/claude-code))
  on your `PATH`, already authenticated (the worker seats shell out to
  `claude --print`). `entire-loop` never handles your API key; `claude`
  authenticates from its own keychain/config.
- **Go 1.26+** and **git** to build the plugin and to run the mutating-build clone.

## Install

`entire-loop` installs the same way as the other Entire plugins — build the
binary and register it with `entire plugin install`. The bundled installer
chains the siblings first (graph, then brain), then the loop:

```sh
git clone https://github.com/suhaanthayyil/entire-loop
cd entire-loop
sh scripts/install.sh
```

`scripts/install.sh` verifies `entire` is on `PATH`, ensures the `graph` and
`brain` plugins are installed and reachable (building them from sibling checkouts
if you have them — see below), builds and registers `entire-loop`, then runs
`entire loop doctor`.

The siblings are expected next to this repo (`../entire-graph`, `../entire-brain`),
or point the installer at them explicitly:

```sh
ENTIRE_GRAPH_DIR=/path/to/entire-graph \
ENTIRE_BRAIN_DIR=/path/to/entire-brain \
  sh scripts/install.sh
```

If a sibling is neither installed nor checked out, the installer stops with a
clear message and the `git clone` command to run. To (re)register only the loop
binary once the siblings are in place:

```sh
sh scripts/install-local.sh
```

## Populate the brain (important)

Brain-backed briefs need a **built brain**. The graph provider works
**statelessly** — `entire graph symbols`/`edges` parse your working tree with no
setup — but the brain must be indexed for a repo before it returns durable facts,
history, and docs. In the repo you want to loop over, run:

```sh
entire brain refresh          # build/refresh the brain for this repo
entire brain status           # confirm sources are indexed
```

`entire-loop` **degrades gracefully** when the brain is empty or unreachable:
each brief simply carries a "(brain unavailable)" note instead of durable
context, and the loop still runs on the graph alone. `entire loop doctor` prints
a hint to run `entire brain refresh` when it detects the brain has no data
indexed for the current repo.

## Usage

```sh
entire loop "make the flaky ingest test deterministic"
entire loop run "add a --dry-run flag to the importer" --rounds 2 --jobs 3
entire loop status            # recent runs + their state
entire loop doctor            # env + sibling (graph/brain) reachability + brain-data hint
entire loop version --json
```

`entire loop "<goal>"` is shorthand for `entire loop run "<goal>"`.

### Flags (`run`)

| Flag | Default | Meaning |
|---|---|---|
| `--planner` | `llm` | Round planner: `llm` (self-planning control plane — an LLM control seat plans and re-plans each round; adds one control-seat cost per round) or `fixed` (static research/build/critic/measure roster) |
| `--rounds` | `0` | Fixed round count (`0` = converge until dry); also caps `--max-rounds` |
| `--max-rounds` | `6` | Safety cap on rounds in converge mode |
| `--jobs` | `2` | Max concurrent worker seats (the concurrency cap) |
| `--allow-mutating-build` | `false` | Let the build seat write real code in an isolated throwaway clone (see Security). Off = plan-mode propose-as-text only |
| `--repo` | `ENTIRE_REPO_ROOT` → git discovery | Repository root the loop reasons over |
| `--model` | *(agent default)* | Worker model override (`claude --model`) |
| `--effort` | *(agent default)* | Worker reasoning effort (`low`/`medium`/`high`) |

Run state is persisted to `$ENTIRE_PLUGIN_DATA_DIR/runs/<runID>/state.json`
(falling back to `~/.local/share/entire/plugins/data/loop` when the env var is
unset), rewritten after every round. `entire loop status` reads it back.

## How it uses brain + graph

Each seat's **brief** is assembled from two sources and folded into the seat's
role template:

- **`entire brain query "<goal>"`** — hybrid (lexical + vector, RRF) retrieval of
  durable facts, prior decisions, history, and docs.
- **`entire graph symbols --repo <root> --format ndjson`** — the code structure
  (definitions) around the goal, bounded to a head so a large repo can't blow the
  brief budget. (`entire graph edges` — callers/callees — is available for
  relationship-heavy work; the research seat additionally binds the brain MCP,
  which exposes the same graph.)

Wiring is **hybrid per seat**:

- **Deep seats** (research, critic) bind the `entire-brain` MCP server
  (`claude --mcp-config … entire brain mcp`) and get `ENTIRE_REPO_ROOT` in their
  env so the brain binds the target repo. Default model: `sonnet`.
- **Cheap seats** (build, measure, verifiers, control) run with MCP fully **off**
  (`--strict-mcp-config` + empty server set) and work from the pre-assembled brief
  only. Default model: `claude-haiku-4-5`.

Every shell-out is bounded and time-boxed: a slow, hung, or missing sibling
surfaces a graceful note in the brief instead of stalling or failing the loop.

## ⚠️ Security / trust boundary

**The mutating build seat runs a coding agent in bypassPermissions mode.** It is
**OFF by default** and only ever runs when you pass `--allow-mutating-build`. When
enabled, the build seat spawns a `claude` worker in **`bypassPermissions`** mode —
it **can write files, run shell commands, and reach the network**. Understand the
boundary before you enable it:

**What confines it:**

- It runs in a **throwaway `git clone --local --no-hardlinks`** of your repo,
  created in the OS temp dir — **never** a git worktree (an earlier worktree-based
  version was a security bug: a worktree shares the real repo's object store).
  The clone has its **own object store and refs**, so nothing the worker does to
  objects, refs, tags, or branches can reach your repository.
- The clone's `origin` remote (which points at your repo's absolute path) is
  **removed**, and the seat's brief/graph are bound to the clone dir — so the
  **real repo path is never handed to the worker** in its cwd, env, brief, or git
  config.
- The worker's environment is **scrubbed** down to a minimal allowlist:
  `ANTHROPIC_API_KEY`, `AWS_*`, `GOOGLE_*`, `GITHUB_TOKEN`, and the generic
  `*_TOKEN`/`*_SECRET`/`*_KEY` families are **stripped** and cannot be
  exfiltrated. `claude` still authenticates from its own keychain/config.
- The loop captures the clone's `git diff` as the proposal and **discards the
  clone** on every normal, cancel, and crash path (orphans are age-swept on the
  next run). Your working tree, refs, and tags are never touched.

**What does NOT confine it:**

- **There is no OS sandbox.** `bypassPermissions` means a compromised or
  adversarial worker could still reach the **network** or **absolute filesystem
  paths** outside the clone. **Do not point `--allow-mutating-build` at untrusted
  goals or untrusted repositories.**

**Privilege lock:** the LLM planner **cannot** enable the mutating seat. Whether a
seat is mutating is derived **solely** from the human `--allow-mutating-build`
flag AND `role == build` — never from planner output. No plan, however
adversarial, can escalate a seat into a bypass worker.

**Default posture:** without the flag, *every* seat (including build) runs in
Claude Code **plan mode** and only reads, analyzes, and **proposes** — it never
edits the target repo.

## Config / environment

| Variable | Set by | Effect |
|---|---|---|
| `ENTIRE_REPO_ROOT` | parent `entire` (or you) | Target repo root; the fallback before `--repo` and git discovery |
| `ENTIRE_PLUGIN_DATA_DIR` | parent `entire` | Where run state is written (`runs/<id>/state.json`); XDG fallback otherwise |
| `ENTIRE_CLI_VERSION` | parent `entire` | Reported by `entire loop doctor` |
| `ENTIRE_BRAIN_NO_EGRESS` / `ENTIRE_BRAIN_LOCAL_ONLY` | you | **Fail-closed** local-only mode: the `claude` worker is refused (it can send selected brain context off the local loopback), so a run exits non-zero rather than leak. No local-loopback worker is implemented yet |

## Reliability

- **Process-group reaping.** Each worker runs in its own process group; on
  timeout or cancellation the whole group is signaled (SIGTERM, then SIGKILL
  after a grace) so `node`/`claude` grandchildren never leak. After SIGKILL a
  bounded reap grace applies: a child stuck in uninterruptible sleep can never
  hang the round.
- **Per-worker wall-clock timeout.**
- **Graceful degradation.** A garbled/missing worker envelope, a sibling being
  down, a seat panic, or a truncated worker output degrades the round with a
  warning — it never aborts. A round entirely refused under no-egress exits
  non-zero.
- **Isolated mutation.** The build seat writes code only inside a throwaway
  clone; the target's working tree, refs, and tags are never modified.
- **Round-scoped idempotent skip.** A completed *OK* seat result is cached under
  the run dir keyed by round, so a re-run reuses it but round N never replays
  round N-1. Failed results are never cached.

## License

MIT — see [LICENSE](LICENSE).
