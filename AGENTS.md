# forgejo-berth

A broker that gives each waiting Forgejo Actions job an ephemeral runner on a container slot of
a fixed host, admitted by the job's learned memory peak. One Go binary: `run` (the broker),
`slot` (the forced SSH command on a CI host), `cycle` (one slot's reset cycle). The design is
[docs/design.md](docs/design.md); keep it true when behaviour changes.

## Layout

- `cmd/forgejo-berth/`: the subcommands.
- `internal/config/`: the broker's YAML configuration, its defaults and checks.
- `internal/broker/`: jobs, offers, predictions, the scheduler (`schedule.go`) and the loop
  (`broker.go`), the state and metrics (`state.go`).
- `internal/history/`: SQLite (pure Go, `modernc.org/sqlite`). Its schema is an interface
  (shadow mode reads a live instance's database): add columns, never rename or drop them.
- `internal/forgejo/`: the Forgejo API client.
- `internal/provider/`: the slot provider interface, the SSH provider, the protocol's checks.
- `internal/slot/`: the host side: the slot command, the reset cycle, Incus.
- `e2e/`: the broker against a fake Forgejo API and an in-memory provider.
- `examples/`: configuration, slot settings, units. `scripts/`: the release build.

## Rules

- Behaviour parity of the learning and scheduling rules matters more than features: a change
  to `Schedule`, `Predict`, `Offered` or the history's estimate comes with a table case.
- No runner token in logs, state, history or errors. `provider.Handoff` formats without it.
- Secrets come from files, never from flags or the environment.
- The slot protocol stays compatible: new fields are optional, old ones keep their meaning.
- Dependencies stay minimal: the standard library where reasonable; the binary must build
  with `CGO_ENABLED=0`.
- SPDX header `// SPDX-License-Identifier: GPL-3.0-or-later` on every source file.
- Commits are Conventional Commits (`feat:`, `fix:`, `docs:`, `ci:`, `test:`, `chore:`);
  releases are tagged from them and CHANGELOG.md is written with them.
- Markdown: lines of at most 100 characters (tables and code excepted), tables aligned.

## Checks

```sh
test -z "$(gofmt -s -l .)"
go vet ./...
golangci-lint run
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
npx --yes markdownlint-cli2 "**/*.md"
```

The tests start `httptest` servers on loopback; where a sandbox refuses loopback connections,
run them in a container (`docker run --rm -v "$PWD":/src -w /src golang:1.27 go test -race ./...`).
