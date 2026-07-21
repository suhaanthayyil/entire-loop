#!/usr/bin/env bash
# End-to-end installer for the entire-loop plugin.
#
# entire-loop depends on two sibling plugins at runtime: entire-graph (structural
# context) and entire-brain (durable knowledge + MCP). This script:
#   1. verifies the parent `entire` CLI is on PATH,
#   2. ensures the `graph` and `brain` plugins are installed and reachable —
#      building them from sibling checkouts when they are missing, or telling you
#      exactly how to clone a dep that is neither installed nor checked out,
#   3. builds and registers entire-loop, then
#   4. runs `entire loop doctor`.
#
# Sibling checkouts are expected next to this repo (../entire-graph,
# ../entire-brain); override with ENTIRE_GRAPH_DIR / ENTIRE_BRAIN_DIR.
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

graph_dir=${ENTIRE_GRAPH_DIR:-"$repo_root/../entire-graph"}
brain_dir=${ENTIRE_BRAIN_DIR:-"$repo_root/../entire-brain"}
graph_url="https://github.com/entireio/entire-graph"
brain_url="https://github.com/entireio/entire-brain"

err() { printf 'error: %s\n' "$1" >&2; }

if ! command -v entire >/dev/null 2>&1; then
	err 'the parent `entire` CLI is required; install it first (https://github.com/entireio)'
	exit 1
fi

# ensure_sibling installs a sibling plugin from its checkout when the plugin is
# not already reachable. Args: label, checkout dir, git URL, and the probe
# command that reports reachability (passed as the remaining args).
ensure_sibling() {
	label=$1
	dir=$2
	url=$3
	shift 3

	printf '==> Checking sibling plugin: %s\n' "$label"
	if "$@" >/dev/null 2>&1; then
		printf '    %s: reachable\n' "$label"
		return 0
	fi

	printf '    %s: not reachable — attempting install from %s\n' "$label" "$dir"
	if [ -x "$dir/scripts/install-local.sh" ]; then
		( cd "$dir" && sh scripts/install-local.sh )
	elif [ -d "$dir" ]; then
		err "the $label checkout at $dir has no scripts/install-local.sh; update or reinstall it"
		exit 1
	else
		err "the \`$label\` plugin is not installed and no checkout was found at $dir."
		printf 'hint: clone it next to this repo (or set the *_DIR override) and re-run:\n' >&2
		printf '        git clone %s "%s"\n' "$url" "$dir" >&2
		exit 1
	fi

	if ! "$@" >/dev/null 2>&1; then
		err "installed $label but it is still not reachable; check the install output above"
		exit 1
	fi
	printf '    %s: installed and reachable\n' "$label"
}

# Order matters: graph first (brain builds on the graph provider), then brain.
ensure_sibling graph "$graph_dir" "$graph_url" entire graph doctor --json
ensure_sibling brain "$brain_dir" "$brain_url" entire brain status

printf '==> Building and registering entire-loop\n'
sh "$repo_root/scripts/install-local.sh"

printf '==> Verifying the loop plugin environment\n'
entire loop doctor

cat <<'DONE'

entire-loop is installed:
  - plugin built and registered with the parent Entire CLI
  - siblings (graph, brain) verified reachable

Next:
  - build a brain (per repo):  cd <your-repo> && entire brain refresh
  - run a loop:                entire loop "your goal here"
  - inspect runs:              entire loop status
  - re-check the environment:  entire loop doctor
DONE
