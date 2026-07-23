# graph-loop — Codex shim for entire-loop

Thin Codex plugin over ONE core: the `entire-loop` Go binary, invoked as
`entire loop`. No orchestration logic lives here — this plugin only exposes the
core to Codex as a custom prompt/command that shells the same binary every other
runtime uses. See `../docs/any-agent.md` for the runtime-agnostic contract.

## Contents

| Path | Role |
|---|---|
| `commands/graph-loop.toml` | Codex command (`description = …` + `prompt = …`, `{{args}}` = goal) that instructs Codex to run `entire loop "<goal>"`. Matches the caveman `.codex` `commands/*.toml` shape. |

### No `hooks.json`

`graph-loop` is a **command-invoked** action, not a persistent session mode, so it
needs no hook (and therefore no `.codex/config.toml` `[features] hooks = true`
either). Caveman ships `.codex/hooks.json` only because it activates a mode at
`SessionStart`; a command-only plugin does not. Nothing to invent here.

## Prerequisites (on PATH)

- **`entire`** — parent Entire CLI; the loop runs as `entire loop`.
- **`entire-graph`** (the `graph` plugin) — **required** structural provider.
- **`entire-brain`** (the `brain` plugin) — **optional**; auto-used when present,
  degrades to graph + goal otherwise.

Run `entire loop doctor` to confirm env + sibling reachability before first use.

## Install / use (needs the Codex runtime — not done in this repo)

Install the plugin per your Codex plugin flow, then invoke the command with a
goal as its argument:

```
/graph-loop make the flaky ingest test deterministic
/graph-loop add a --dry-run flag to the importer --rounds 2 --jobs 3
```

`{{args}}` is substituted into `entire loop "<goal>"` (shorthand for
`entire loop run "<goal>"`). Flags: `--planner llm|fixed`,
`--plan-mode dynamic|immutable`, `--rounds N`, `--max-rounds N`, `--jobs K`,
`--measure-cmd "<cmd>"`, `--allow-mutating-build`, `--repo`, `--model`,
`--effort`.

## Security

`--allow-mutating-build` is OFF by default. When set, the build seat runs a
`claude` worker in `bypassPermissions` mode inside a throwaway `git clone --local
--no-hardlinks` (own object store/refs, scrubbed env, discarded on every path) —
your working tree is never touched, but there is **no OS sandbox**. Do not point
it at untrusted goals or repositories. See the root `README.md` for the full
trust boundary.
