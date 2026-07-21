#!/usr/bin/env bash
# End-to-end installer for the entire-loop plugin.
#
# entire-loop depends on one REQUIRED sibling plugin at runtime, entire-graph
# (structural context), and auto-uses one OPTIONAL sibling, entire-brain (durable
# knowledge + MCP) when it is present. entire-graph is public; entire-brain is
# currently private, so public users cannot install it — the loop runs fine
# graph-backed without it. This script:
#   1. verifies the parent `entire` CLI is on PATH,
#   2. ensures the `graph` plugin is installed and reachable — building it from a
#      sibling checkout when missing, or telling you exactly how to clone it (and
#      failing the install, since graph is required),
#   3. tries to ensure the `brain` plugin the same way, but treats it as optional:
#      if brain is unavailable (private/no checkout/not reachable) it prints a
#      one-line note and CONTINUES rather than failing,
#   4. builds and registers entire-loop, then
#   5. runs `entire loop doctor`.
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

# ensure_sibling_optional mirrors ensure_sibling for an OPTIONAL sibling: it tries
# the same reachable-or-install-from-checkout path, but a checkout, install, or
# reachability failure never aborts the script — it prints a one-line note and
# moves on. This is what lets brain (private; most users cannot install it) sit
# alongside graph (required) without blocking a public install. Every step that
# could fail is inside an `if`/`&&` test (never a bare statement), so nothing here
# can trip `set -e`; the function always returns 0. Sets brain_status to
# "reachable"/"installed"/"unavailable" for the closing summary.
ensure_sibling_optional() {
	label=$1
	dir=$2
	url=$3
	shift 3

	printf '==> Checking optional sibling plugin: %s\n' "$label"
	if "$@" >/dev/null 2>&1; then
		printf '    %s: reachable\n' "$label"
		brain_status="reachable"
		return 0
	fi

	if [ -x "$dir/scripts/install-local.sh" ]; then
		printf '    %s: not reachable — attempting install from %s\n' "$label" "$dir"
		if ( cd "$dir" && sh scripts/install-local.sh ) && "$@" >/dev/null 2>&1; then
			printf '    %s: installed and reachable\n' "$label"
			brain_status="installed"
			return 0
		fi
	fi

	printf 'note: %s (optional) not available — entire-loop will run graph-backed; install entire-%s later to enrich briefs\n' "$label" "$label"
	brain_status="unavailable"
	return 0
}

# Order matters: graph first (brain, when present, builds on the graph provider).
# graph is REQUIRED — the install fails with a `git clone` hint if it can't be
# made reachable. brain is OPTIONAL (entire-graph is public, entire-brain is
# currently private) — it is auto-used when present and skipped with a note
# otherwise; the loop always runs fine graph-backed.
brain_status="unavailable"
ensure_sibling graph "$graph_dir" "$graph_url" entire graph doctor --json
ensure_sibling_optional brain "$brain_dir" "$brain_url" entire brain status

printf '==> Building and registering entire-loop\n'
sh "$repo_root/scripts/install-local.sh"

printf '==> Verifying the loop plugin environment\n'
entire loop doctor

printf '\nentire-loop is installed:\n'
printf '  - plugin built and registered with the parent Entire CLI\n'
printf '  - graph (required) verified reachable\n'
if [ "$brain_status" = "unavailable" ]; then
	printf '  - brain (optional) not available — running graph-backed; install entire-brain later to enrich briefs\n'
else
	printf '  - brain (optional) verified reachable\n'
fi

cat <<'DONE'

Next:
  - run a loop:                entire loop "your goal here"
  - inspect runs:              entire loop status
  - re-check the environment:  entire loop doctor
DONE
if [ "$brain_status" != "unavailable" ]; then
	printf '  - build a brain (per repo, if you have it):  cd <your-repo> && entire brain refresh\n'
fi
