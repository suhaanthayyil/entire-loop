# entire-loop — any-agent contract

The runtime-agnostic contract for shelling the entire-loop core from ANY
agent/harness. There is exactly **one core**: the `entire-loop` Go binary,
invoked as `entire loop`. Every runtime (the Entire root plugin, the Claude Code
plugin, the Codex plugin, and any bespoke harness) is a **thin shim** that shells
this same command — no orchestration logic is duplicated per runtime. If your
harness can run a shell command and read files, it can drive the loop.

## Exact command

```sh
entire loop "<goal>" [flags]
```

`entire loop "<goal>"` is shorthand for `entire loop run "<goal>"`.
Subcommands: `run` (default), `status`, `doctor`, `version`.

### Flags (`run`)

| Flag | Default | Meaning |
|---|---|---|
| `--planner llm\|fixed` | `llm` | HOW a round is planned: self-planning LLM control plane vs static roster |
| `--plan-mode dynamic\|immutable` | `dynamic` | WHEN the DAG is planned: re-plan every round from state vs plan once up front and execute it every round with bounded per-node recovery |
| `--rounds N` | `0` | Fixed round count (`0` = converge until dry); also caps `--max-rounds` |
| `--max-rounds N` | `6` | Safety cap on rounds in converge mode |
| `--jobs K` | `2` | Max concurrent worker seats |
| `--measure-cmd "<cmd>"` | *(off)* | Read-only external command; its JSON stdout (e.g. `{"progress":0.8,"risk":0.1}`) parses into typed round metrics that OVERRIDE the model-derived ones, so the loop converges on a real external signal |
| `--allow-mutating-build` | `false` | Let the build seat write real code in an isolated throwaway clone (see Security). Off = propose-as-text only |
| `--repo <path>` | `ENTIRE_REPO_ROOT` → git discovery | Repository root the loop reasons over |
| `--model` | *(agent default)* | Worker model override (`claude --model`) |
| `--effort low\|medium\|high` | *(agent default)* | Worker reasoning effort |

## Required environment

| Variable | Set by | Effect |
|---|---|---|
| `ENTIRE_REPO_ROOT` | parent `entire` (or you) | Target repo root; the fallback before `--repo` and git discovery |
| `ENTIRE_PLUGIN_DATA_DIR` | parent `entire` (or you) | Where run state is written (`runs/<id>/state.json`) |
| `ENTIRE_CLI_VERSION` | parent `entire` | Reported by `entire loop doctor` (informational) |

**Host-set under the Entire CLI:** when the loop runs as `entire loop` (the
parent Entire CLI dispatching this plugin), `entire` supplies `ENTIRE_REPO_ROOT`,
`ENTIRE_PLUGIN_DATA_DIR`, and `ENTIRE_CLI_VERSION` automatically — a shim need set
nothing.

**Standalone (not under the Entire CLI):** export them yourself, e.g.

```sh
export ENTIRE_REPO_ROOT="$(git rev-parse --show-toplevel)"
export ENTIRE_PLUGIN_DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/entire/plugins/data/loop"
entire loop "<goal>"
```

Both are optional in practice: if `ENTIRE_REPO_ROOT` is unset the core falls back
to `--repo` then git discovery; if `ENTIRE_PLUGIN_DATA_DIR` is unset it falls back
to `$XDG_DATA_HOME/entire/plugins/data/loop`, else
`~/.local/share/entire/plugins/data/loop`.

## Dependencies (on PATH)

- **`entire`** — the parent Entire CLI. **Required**: the plugin runs as
  `entire loop`.
- **`entire-graph`** (the `graph` plugin) — **required** structural provider
  (`entire graph symbols|edges`). Every seat's brief is built from it.
- **`entire-brain`** (the `brain` plugin) — **optional** durable-knowledge
  enhancement. Auto-used when installed and reachable; degrades gracefully to
  graph + goal otherwise. Deep seats bind it as an MCP server (`entire brain
  mcp`); when absent, the brief carries a "(brain context unavailable)" note.

Preflight with `entire loop doctor` — it reports the supplied env and whether
`graph`/`brain` are reachable.

## Output / state contract

- **Run state** is persisted to `$ENTIRE_PLUGIN_DATA_DIR/runs/<runID>/state.json`
  (XDG fallback as above), rewritten after every round. `entire loop status`
  reads it back and lists recent runs.
- **Proposed diffs**: by default every seat runs in plan mode and only reads,
  analyzes, and **proposes** (text) — the target repo is never edited. With
  `--allow-mutating-build`, the build seat writes real code inside a throwaway
  clone and the loop captures that clone's `git diff` as the proposal; the clone
  is discarded on every normal, cancel, and crash path. Your working tree, refs,
  and tags are never modified.

### Exit codes

- **`0`** — the loop ran (converged, hit `--rounds`, or hit the `--max-rounds`
  cap). Individual degraded seats (missing/garbled worker output, a sibling being
  down, a seat panic) do **not** fail the run — they degrade the round with a
  warning.
- **non-zero** — a hard failure. In particular, a run entirely **refused under
  no-egress** exits non-zero rather than leaking: set
  `ENTIRE_BRAIN_NO_EGRESS=1` (or `ENTIRE_BRAIN_LOCAL_ONLY=1`) for fail-closed
  local-only mode — the `claude` worker is refused (it could send selected brain
  context off the local loopback), so the run exits non-zero. Treat a non-zero
  exit as terminal; surface it rather than retrying.

## Knobs a harness should expose

- **`--plan-mode dynamic|immutable`** — adaptivity vs predictability. `dynamic`
  re-plans the seat DAG every round from state (can re-route around dead ends,
  non-deterministic plan); `immutable` plans the DAG once up front, disables
  runtime reorg, and recovers node failures in place within a bounded budget
  (reproducible, cannot change strategy mid-run). Both are bounded by
  `--max-rounds` and the same integrity rules (privilege lock, no-egress gate,
  verifier gate).
- **`--measure-cmd "<cmd>"`** — the external measure edge. Off by default,
  timeout-bounded, and must be **read-only** (the loop never grants it write
  privilege and never feeds it agent output). It must print a JSON object of
  numeric metrics (e.g. `{"progress":0.8,"risk":0.1}`) to stdout; those override
  the model-derived round metrics so the loop converges on a signal you can
  trust. Non-numeric fields are ignored; a failure (bad exit, no JSON, no numeric
  metric) degrades gracefully — the round keeps its model metrics and logs the
  failure.

## Security / trust boundary (`--allow-mutating-build`)

**OFF by default.** Without it, *every* seat (including build) runs in plan mode
and only proposes. When enabled, the build seat spawns a `claude` worker in
**`bypassPermissions`** mode — it can write files, run shell, and reach the
network.

What confines it: a throwaway **`git clone --local --no-hardlinks`** in the OS
temp dir (its own object store and refs — never a git worktree), `origin`
removed, the real repo path never handed to the worker, the worker env scrubbed
to a minimal allowlist (`*_TOKEN`/`*_SECRET`/`*_KEY` families stripped), and the
clone discarded on every path.

What does NOT confine it: **there is no OS sandbox** — a hostile worker could
still reach the network or absolute paths outside the clone. **Do not point
`--allow-mutating-build` at untrusted goals or repositories.**

**Privilege lock:** the LLM planner **cannot** enable the mutating seat. Whether a
seat is mutating is derived **solely** from the human `--allow-mutating-build`
flag AND `role == build` — never from planner output. No plan can escalate a seat
into a bypass worker.
