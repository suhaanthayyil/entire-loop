# Contributing to entire-loop

Thanks for your interest. entire-loop is a small Go project; contributions are
welcome via pull request.

## Development

Requires Go 1.26+, git, and (for a full local run) the `entire` CLI with the
`graph` and `brain` sibling plugins plus a logged-in `claude`. See the
[README](README.md) for the plugin prerequisites.

Before opening a PR, keep the gates green:

```sh
gofmt -w .
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

The unit tests never spawn a real `claude`, sibling plugin, or network call — the
runner, briefer, and planner are all injectable and stubbed in tests. Please keep
new tests hermetic (no real `claude`/`git`-network/`~/.config` access) and add
coverage for behavior changes.

## Guidelines

- Keep the mutating-build trust boundary intact: the `Mutating` bit must remain
  derived solely from the human `--allow-mutating-build` flag and `role == build`
  (see `lockMutating`) — never from planner or worker output.
- Prefer graceful degradation over hard failure for anything that shells out to a
  sibling plugin or worker.
- Conventional-commit messages (`feat:`, `fix:`, `docs:`, …).
