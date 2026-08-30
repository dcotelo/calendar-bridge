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
make tools     # installs the pinned golangci-lint and govulncheck
make ci        # everything CI runs
```

`make help` lists every target. The useful ones:

| Target | What it does |
|---|---|
| `make build` | Build into `bin/`, with version ldflags |
| `make test` / `make test-race` | The test suite |
| `make cover` | Tests plus the coverage gates |
| `make lint` | golangci-lint |
| `make vuln` | govulncheck |
| `make fuzz` | Each fuzz target briefly (`FUZZTIME=30s` to change the budget) |
| `make ci` | All of the above, exactly as CI runs them |
| `make screenshots` | Regenerate the docs screenshots from a synthetic fixture |
| `make demos` | Regenerate the terminal GIFs (needs [vhs](https://github.com/charmbracelet/vhs)) |

No external services are needed to build or test. The Calendar API sits behind
`internal/sync.CalendarClient`, and the suite uses an in-memory fake
(`internal/sync/fake_client_test.go`) plus an `httptest` double that speaks the
real Calendar API wire format (`internal/sync/google_client_test.go`). You only
need real Google credentials to run against live accounts, never to develop or
test.

## Before opening a PR

```bash
make ci
```

A green `make ci` means a green pipeline — the CI workflow runs the same
targets. It covers build, vet, gofmt, `go.mod` tidiness, `go test -race`, the
coverage gates, golangci-lint and govulncheck.

Coverage is gated as an overall floor plus a per-package floor for each package
that carries logic. `cmd/calendar-bridge` is deliberately ungated — it is
process plumbing (flag parsing, signal handling, `os.Exit`) whose unit tests
would assert almost nothing.

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
- PR titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  and are linted, because `CHANGELOG.md` is generated from them.

## Recommended branch protection

Not yet enforced on this repo; documented so it can be, and so a fork knows the
intent. On `main`:

- Require a pull request before merging, with at least one approving review.
- Require these status checks to pass: `test`, `gofmt`, `coverage`, `lint`,
  `vulncheck`, `fuzz`, `docker build`, `markdown links`, `CodeQL`.
- Require branches to be up to date before merging.
- Require conversation resolution before merging.
- Dismiss stale approvals when new commits are pushed.
- Do not allow force pushes or deletions.
- Include administrators.

## Where things live

| Path | What it is |
|---|---|
| `cmd/calendar-bridge` | CLI entry point (`auth`, `sync-once`, `run`, `ui`, `version`) |
| `internal/sync` | The engine: fetch, propagate, garbage-collect |
| `internal/sync/client.go` | `CalendarClient` + the real Google-backed implementation |
| `internal/sync/provider.go` | The provider-neutral seam a non-Google backend implements |
| `internal/googleauth` | OAuth2 flow and token persistence |
| `internal/config` | Config loading, validation, saving |
| `internal/webui` | The loopback configuration UI |
| `internal/webhook` | Push receiver, debouncer, watch-channel manager |
| `internal/metrics` | Prometheus exposition and health probes |
| `internal/atomicfile` | `0600` temp-fsync-rename writes for config and tokens |
| `deploy/` | systemd, launchd, docker-compose, Kubernetes |
| `docs/adr/` | Decisions worth not re-litigating |

Start with [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how a pass works,
and [QUALITY_AUDIT.md](QUALITY_AUDIT.md) for the invariants `internal/sync` must
uphold and how each is tested.

## Reporting bugs / requesting features

Use the issue templates. For security issues, see [SECURITY.md](SECURITY.md)
instead of opening a public issue.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
