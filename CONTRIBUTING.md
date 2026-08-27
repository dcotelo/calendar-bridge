# Contributing to calendar-bridge

Thanks for considering a contribution. This is a small, early-stage project,
so the process is intentionally lightweight.

## Before you start

- For anything beyond a small fix (new features, architecture changes),
  open an issue first to discuss the approach. Saves you from writing code
  that goes a direction the project doesn't want.
- Check open issues and PRs so you're not duplicating work.

## Development setup

Requires Go 1.25+.

```bash
git clone https://github.com/dcotelo/calendar-bridge.git
cd calendar-bridge
go build ./...
go test ./...
```

No external services are required to build or run the test suite — the
Calendar API is abstracted behind `internal/sync.CalendarClient`, and tests
use an in-memory fake (`internal/sync/fake_client_test.go`). You only need
real Google OAuth credentials to run calendar-bridge against live accounts,
not to develop or test it.

## Before opening a PR

```bash
go build ./...
go vet ./...
go test ./... -race
gofmt -l .        # must print nothing
```

All four are enforced in CI (`.github/workflows/ci.yml`) and will block
merge if they fail.

## Pull request guidelines

- Branch off `main`; PRs merge into `main`. Direct pushes to `main` aren't
  used, even by maintainers — everything goes through review.
- Keep PRs focused. A PR that does one thing is easier to review than one
  that mixes a feature with an unrelated refactor.
- Add or update tests for behavior you change. `internal/sync` is the
  core logic and the least forgiving of missing coverage.
- [CodeRabbit](https://coderabbit.ai) reviews every PR automatically — see
  `.coderabbit.yaml` for the specific invariants it checks, especially
  around `internal/googleauth` (credential handling) and `internal/sync`
  (the owned-block tagging invariant that prevents sync loops and
  accidental overwrites of real events).
- Commits are signed (`git commit -S` or SSH signing — see
  [GitHub's docs](https://docs.github.com/en/authentication/managing-commit-signature-verification)).
  Not currently enforced by branch protection, but appreciated.

## Where things live

| Path | What it is |
|---|---|
| `internal/config` | YAML config loading and validation |
| `internal/googleauth` | OAuth2 flow and token persistence |
| `internal/sync` | The core engine: fetch, propagate, garbage-collect |
| `internal/sync/client.go` | `CalendarClient` interface + real Google-backed implementation |
| `cmd/calendar-bridge` | CLI entry point (`auth`, `sync-once`, `run`) |

## Reporting bugs / requesting features

Use the issue templates. For security issues, see [SECURITY.md](SECURITY.md)
instead of opening a public issue.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
