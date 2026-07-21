#!/bin/sh
# Build the entire-loop plugin binary and register it with the parent Entire CLI.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

if ! command -v entire >/dev/null 2>&1; then
	printf 'error: the parent `entire` CLI is required for plugin installation\n' >&2
	exit 1
fi

version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')}
go build -trimpath -ldflags "-X main.version=$version" -o entire-loop ./cmd/entire-loop
entire plugin install ./entire-loop --force
entire loop version
