#!/bin/sh
# End-to-end installer for the entire-loop plugin.
#
# entire-loop depends on two sibling plugins at runtime: entire-graph (structural
# context) and entire-brain (durable knowledge + MCP). This script verifies both
# are reachable, then builds and registers entire-loop.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

if ! command -v entire >/dev/null 2>&1; then
	printf 'error: the parent `entire` CLI is required; install it first\n' >&2
	exit 1
fi

printf '==> Verifying sibling plugins\n'
if entire graph doctor --json >/dev/null 2>&1; then
	printf '    graph: reachable\n'
else
	printf 'error: `entire graph doctor --json` failed; install the entire-graph plugin first\n' >&2
	exit 1
fi
if entire brain status >/dev/null 2>&1; then
	printf '    brain: reachable\n'
else
	printf 'error: `entire brain status` failed; install the entire-brain plugin first\n' >&2
	exit 1
fi

printf '==> Building and registering entire-loop\n'
sh "$repo_root/scripts/install-local.sh"

printf '==> Verifying the loop plugin environment\n'
entire loop doctor

cat <<'DONE'

entire-loop is installed:
  - plugin built and registered with the parent Entire CLI
  - siblings (graph, brain) verified reachable

Next:
  - run a loop:  entire loop "your goal here"
  - inspect:     entire loop status
  - re-check:    entire loop doctor
DONE
