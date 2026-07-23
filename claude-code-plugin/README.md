# graph-loop — Claude Code shim for entire-loop

Thin Claude Code plugin over ONE core: the `entire-loop` Go binary, invoked as
`entire loop`. No orchestration logic lives here — this plugin only exposes the
core to Claude Code as a skill, a slash command, and an optional MCP wiring. The
same binary is shelled by every runtime (Entire root plugin, Codex, any agent);
see `../docs/any-agent.md` for the runtime-agnostic contract.

## Contents

| Path | Role |
|---|---|
| `.claude-plugin/plugin.json` | Plugin manifest (`name: graph-loop`). |
| `.claude-plugin/marketplace.json` | Self-marketplace, one entry, `source: "./"`. |
| `skills/graph-loop/SKILL.md` | When-to-use + how to run the loop, flags, loops→graphs framing, `--allow-mutating-build` security note. |
| `commands/graph-loop.md` | `/graph-loop <goal>` → runs `entire loop "$ARGUMENTS"` via Bash (`allowed-tools: [Bash]`). |
| `.mcp.json` | Declares the optional `entire-brain` MCP server (`entire brain mcp`). |

## Prerequisites (on PATH)

- **`entire`** — parent Entire CLI; the plugin runs as `entire loop`.
- **`entire-graph`** (the `graph` plugin) — **required** structural provider.
- **`entire-brain`** (the `brain` plugin) — **optional**; auto-used when present,
  degrades to graph + goal otherwise.

Run `entire loop doctor` to confirm env + sibling reachability before first use.

## Install (needs the Claude Code runtime — not done in this repo)

```sh
# add this directory as a local marketplace, then install the plugin
/plugin marketplace add /path/to/entire-loop/claude-code-plugin
/plugin install graph-loop@graph-loop
```

## Use

```sh
/graph-loop make the flaky ingest test deterministic
/graph-loop add a --dry-run flag to the importer --rounds 2 --jobs 3
```

`entire loop "<goal>"` is shorthand for `entire loop run "<goal>"`. Flags:
`--planner llm|fixed`, `--plan-mode dynamic|immutable`, `--rounds N`,
`--max-rounds N`, `--jobs K`, `--measure-cmd "<cmd>"`, `--allow-mutating-build`,
`--repo`, `--model`, `--effort`.

## Security

`--allow-mutating-build` is OFF by default. When set, the build seat runs a
`claude` worker in `bypassPermissions` mode inside a throwaway `git clone
--local --no-hardlinks` (own object store/refs, scrubbed env, discarded on every
path) — your working tree is never touched, but there is **no OS sandbox**. Do
not point it at untrusted goals or repositories. See `skills/graph-loop/SKILL.md`
and the root `README.md` for the full trust boundary.
