# entire-loop

A self-planning agent-org **graph loop**, packaged as an Entire CLI plugin.

Where a single agent is one worker on one prompt, `entire-loop` treats a goal as
a small **organization of seats** that runs as a **loop**: an LLM control plane
plans the round, worker seats (research → build → critic → measure/synthesize)
fan out concurrently, the round is verified and synthesized, and the plan is
rewritten from the result — round after round until the goal is met or the round
budget is spent. Loops beat single shots because each round is briefed from the
last, and the org's own structural **graph** — plus durable **brain**, when
available — feed the next plan.

It is a **plugin**, so it composes with the rest of Entire's code intelligence.
Every worker seat is briefed from the sibling `graph` (structural symbols/edges,
**required**) and, when installed, `brain` (durable knowledge, **optional** —
auto-used if present, degrades to graph-only otherwise), with **hybrid per-seat
wiring**: cheap seats read a pre-built brief with MCP off; deep seats bind the
brain MCP server directly when brain is available.

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

`entire-loop` is a plugin on top of the Entire CLI and one required
code-intelligence sibling; a second sibling enriches it but is optional. You
need, on your `PATH`:

- **`entire`** — the parent [Entire CLI](https://github.com/entireio) (`entire`
  must resolve; the plugin runs as `entire loop`).
- **`entire-graph`** *(required)* — the structural provider (symbols + edges),
  installed as the `graph` plugin. Public source:
  <https://github.com/entireio/entire-graph>.
- **`entire-brain`** *(optional)* — the durable knowledge store + MCP tools,
  installed as the `brain` plugin. Source:
  <https://github.com/entireio/entire-brain> — this repo is currently
  **internal/private**, so most public users won't be able to install it yet.
  `entire-loop` **auto-uses brain when it's present** and runs fine without it:
  every brief falls back to graph + goal, so the loop is fully usable
  graph-backed alone.
- **A logged-in agent CLI** — `claude` ([Claude Code](https://www.anthropic.com/claude-code))
  on your `PATH`, already authenticated (the worker seats shell out to
  `claude --print`). `entire-loop` never handles your API key; `claude`
  authenticates from its own keychain/config.
- **Go 1.26+** and **git** to build the plugin and to run the mutating-build clone.

## Install

`entire-loop` installs the same way as the other Entire plugins — build the
binary and register it with `entire plugin install`. The bundled installer
chains the required sibling first (graph), tries the optional one (brain), then
installs the loop:

```sh
git clone https://github.com/suhaanthayyil/entire-loop
cd entire-loop
sh scripts/install.sh
```

`scripts/install.sh` verifies `entire` is on `PATH`, ensures the `graph` plugin
is installed and reachable (building it from a sibling checkout if you have
one — see below; **required**, the install fails if this can't be satisfied),
then tries to do the same for the `brain` plugin (**optional** — if brain's
checkout is missing, its build fails, or it's simply unreachable, the installer
prints a one-line note and continues rather than failing), builds and registers
`entire-loop`, then runs `entire loop doctor`.

The siblings are expected next to this repo (`../entire-graph`, `../entire-brain`),
or point the installer at them explicitly:

```sh
ENTIRE_GRAPH_DIR=/path/to/entire-graph \
ENTIRE_BRAIN_DIR=/path/to/entire-brain \
  sh scripts/install.sh
```

If the **required** `graph` sibling is neither installed nor checked out, the
installer stops with a clear message and the `git clone` command to run. If the
**optional** `brain` sibling is unavailable, the installer notes it and moves on
— entire-loop will run graph-backed. To (re)register only the loop binary once
the siblings you want are in place:

```sh
sh scripts/install-local.sh
```

## Populate the brain (optional, if you have it)

Brain-backed briefs need a **built brain**. The graph provider works
**statelessly** — `entire graph symbols`/`edges` parse your working tree with no
setup — but the brain must be indexed for a repo before it returns durable facts,
history, and docs. If you have `entire-brain` installed, in the repo you want to
loop over, run:

```sh
entire brain refresh          # build/refresh the brain for this repo
entire brain status           # confirm sources are indexed
```

`entire-loop` **degrades gracefully** when the brain is absent, empty, or
unreachable: each brief simply carries a "(brain unavailable)" note instead of
durable context, and the loop still runs on the graph alone. `entire loop doctor`
prints a hint to run `entire brain refresh` when it detects the brain has no data
indexed for the current repo (and reports brain as reachable/not either way,
without failing the check).

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
| `--planner` | `llm` | Round planner (HOW a round is planned): `llm` (self-planning control plane — an LLM control seat plans each round; adds one control-seat cost per round) or `fixed` (static research/build/critic/measure roster) |
| `--plan-mode` | `dynamic` | Plan-mutability axis (WHEN the DAG is planned), orthogonal to `--planner`: `dynamic` (re-plan the seat DAG every round from state — the graph rewrites itself) or `immutable` (plan the DAG **once** up front, disable runtime reorg, and execute it every round with bounded per-node recovery — the fixed-script position). See below. |
| `--measure-cmd` | *(off)* | External measure command. Runs each round under a timeout; its JSON stdout (e.g. `{"progress":0.8}`) parses into typed round metrics that **override** the claude-derived ones, so the loop converges on a **real external signal**. Must be read-only. See below. |
| `--rounds` | `0` | Fixed round count (`0` = converge until dry); also caps `--max-rounds` |
| `--max-rounds` | `6` | Safety cap on rounds in converge mode |
| `--jobs` | `2` | Max concurrent worker seats (the concurrency cap) |
| `--allow-mutating-build` | `false` | Let the build seat write real code in an isolated throwaway clone (see Security). Off = plan-mode propose-as-text only |
| `--repo` | `ENTIRE_REPO_ROOT` → git discovery | Repository root the loop reasons over |
| `--model` | *(agent default)* | Worker model override (`claude --model`) |
| `--effort` | *(agent default)* | Worker reasoning effort (`low`/`medium`/`high`) |

### Plan-mutability axis (`--plan-mode`)

`--planner` decides **how** a round is planned; `--plan-mode` decides **when**. They
are orthogonal — both combine with either planner.

- **`dynamic`** *(default)* — the control seat (or fixed roster) re-plans **every
  round** from the accumulated state, and runtime reorg may reshape the roster. The
  graph rewrites itself as work happens: recovery from a failed node is simply
  re-planning it next round. Best when the shape of the work is discovered as you
  go.
- **`immutable`** — the whole seat/node DAG is planned **once up front** and frozen;
  runtime reorg is disabled and there is no mid-run re-plan. Execution is resilient
  instead of adaptive: a node that degrades is **retried within a bounded budget**,
  then left degraded (bounded per-node recovery). This is the scheduler-theory /
  fixed-script position — reproducible and predictable, at the cost of not adapting
  the plan to what it learns.

The **tradeoff** is adaptivity vs predictability: `dynamic` can re-route around a
dead end but its plan is non-deterministic; `immutable` runs a plan you can read
ahead of time and recovers node failures in place, but cannot change strategy
mid-run. Both are bounded by `--max-rounds` and the same integrity rules
(privilege lock, no-egress gate, verifier gate).

### External measure edge (`--measure-cmd`)

By default the round's metrics are what the **measure seat** (a claude worker)
reasons out. `--measure-cmd` adds a **MeasureEdge** that runs a real command each
round — a test runner, a coverage tool, a benchmark — under a timeout, and parses
its JSON stdout into typed round metrics that **override** the claude-derived ones.
The loop then converges on a signal you can trust rather than one a model asserted:

```sh
entire loop "make the suite pass" --measure-cmd 'go test ./... -json | my-metrics'
# the command must print a JSON object of numeric metrics, e.g. {"progress":0.8,"risk":0.1}
```

Guardrails: it is **off by default** (explicit opt-in), **timeout-bounded**, and
must be **read-only** — the loop never grants it write privilege and never feeds it
agent output. Non-numeric fields are ignored; a failure (bad exit, no JSON, no
numeric metric) degrades gracefully — the round keeps its claude metrics and logs
the failure.

### Edge taxonomy

A round is a graph of **nodes** (agent seats, each its own worker loop) connected by
typed **edges** — functions/predicates over the typed round state that decide the
next node(s) or a gate outcome. The edge set is a small, closed taxonomy:

| Edge | Role |
|---|---|
| **DataEdge** | output → input: carries an upstream seat's validated outcome into a downstream seat's brief (the pipeline edges research→build→critic→…) |
| **ConditionalEdge** | the router: a deterministic predicate on the build proposal's diff size that selects the verify path (solo-critic vs full-audit) |
| **VerifierEdge** | the adversarial gate: N diverse skeptics must fail to refute a large proposal before it is accepted |
| **MeasureEdge** | the external signal (above): runs a command and parses its JSON into typed metrics |
| **CycleEdge** | loop-until-dry: folds each round's outcome into a continue/stop decision (goal-met / dry-streak / fail-streak / cap) |

Run state is persisted to `$ENTIRE_PLUGIN_DATA_DIR/runs/<runID>/state.json`
(falling back to `~/.local/share/entire/plugins/data/loop` when the env var is
unset), rewritten after every round. `entire loop status` reads it back.

## How it uses brain + graph

Each seat's **brief** is assembled from two sources and folded into the seat's
role template — **`graph` is required; `brain` is an optional enhancement**,
auto-used when the `brain` plugin is installed and reachable:

- **`entire graph symbols --repo <root> --format ndjson`** *(required)* — the
  code structure (definitions) around the goal, bounded to a head so a large
  repo can't blow the brief budget. (`entire graph edges` — callers/callees — is
  available for relationship-heavy work; the research seat additionally binds
  the brain MCP, which exposes the same graph, when brain is present.)
- **`entire brain query "<goal>"`** *(optional)* — hybrid (lexical + vector,
  RRF) retrieval of durable facts, prior decisions, history, and docs. When
  brain is absent, unreachable, or has no data indexed for the repo, this
  section of the brief degrades to a graceful "(brain context unavailable)"
  note and the seat still runs on the graph + goal alone.

Wiring is **hybrid per seat**:

- **Deep seats** (research, critic) bind the `entire-brain` MCP server
  (`claude --mcp-config … entire brain mcp`) and get `ENTIRE_REPO_ROOT` in their
  env so the brain binds the target repo, **when brain is installed**. Default
  model: `sonnet`.
- **Cheap seats** (build, measure, verifiers, control) run with MCP fully **off**
  (`--strict-mcp-config` + empty server set) and work from the pre-assembled brief
  only. Default model: `claude-haiku-4-5`.

Every shell-out is bounded and time-boxed: a slow, hung, or missing sibling
surfaces a graceful note in the brief instead of stalling or failing the loop —
this is what makes brain safe to omit entirely.

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

## Multi-runtime

There is **one core** — the `entire-loop` Go binary, invoked as `entire loop`.
Every runtime is a **thin shim** that shells that same command; no orchestration
logic is duplicated per runtime. Add a runtime by adding a shim, not by forking
the loop.

| Runtime | Shim | How it invokes the core |
|---|---|---|
| **Entire CLI** | `entire-plugin.yml` (root plugin) | dispatched as `entire loop …`; supplies `ENTIRE_REPO_ROOT` / `ENTIRE_PLUGIN_DATA_DIR` / `ENTIRE_CLI_VERSION` |
| **Claude Code** | [`claude-code-plugin/`](claude-code-plugin/) | `/graph-loop <goal>` command + `graph-loop` skill run `entire loop "$ARGUMENTS"` via Bash; `.mcp.json` wires the optional `entire-brain` server |
| **Codex** | [`codex-plugin/`](codex-plugin/) | `commands/graph-loop.toml` prompt instructs Codex to run `entire loop "<goal>"` (`{{args}}` = goal) |
| **Any agent / harness** | [`docs/any-agent.md`](docs/any-agent.md) | the runtime-agnostic contract: exact command, required env, deps, output/state + exit-code contract, and the `--plan-mode` / `--measure-cmd` knobs |

All four shell the identical command — `entire loop "<goal>" [flags]` — and share
the same prerequisites (`entire` + `graph` required, `brain` optional), the same
run-state contract (`$ENTIRE_PLUGIN_DATA_DIR/runs/<id>/state.json`), and the same
`--allow-mutating-build` trust boundary described above.

## License

MIT — see [LICENSE](LICENSE).
