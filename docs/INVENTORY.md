# calendar-bridge — repository inventory

Working artifact produced during the Phase 1 audit recon. Everything here was
read out of the source at commit `85c4afc`; nothing is inferred from the README.

## 1. File tree

```
cmd/calendar-bridge/main.go        CLI entry point (447 loc)
internal/config/                   YAML load/save/validate
internal/googleauth/               OAuth2 flow, token persistence, perm checks
internal/sync/                     the engine + Google client + Provider seam + retry
internal/webhook/                  push receiver, debouncer, watch-channel manager
internal/webui/                    loopback config UI (Go handlers + one embedded HTML file)
.github/workflows/{ci,release}.yml
.github/{CODEOWNERS,dependabot.yml,PULL_REQUEST_TEMPLATE.md,ISSUE_TEMPLATE/*}
docs/{web-ui.md,webhooks.md}
Dockerfile, fly.toml, .goreleaser.yml, .golangci.yml, .coderabbit.yaml
config.example.yaml, README.md, CONTRIBUTING.md, SECURITY.md, CODE_OF_CONDUCT.md, LICENSE
```

6,307 lines of Go, of which 2,940 are `_test.go` (47%).

## 2. Package graph

```
cmd/calendar-bridge
  ├── internal/config
  ├── internal/googleauth ──> google.golang.org/api/calendar/v3, golang.org/x/oauth2
  ├── internal/sync       ──> google.golang.org/api/calendar/v3
  ├── internal/webhook    ──> google.golang.org/api/calendar/v3, github.com/google/uuid
  └── internal/webui      ──> internal/config
```

No cycles. `internal/sync` and `internal/webhook` do not import `internal/config`
(good — config stays at the edge). `internal/webui` imports `internal/config`
because it reads and writes `config.yaml` directly.

Direct runtime deps: 4 (`google.golang.org/api`, `golang.org/x/oauth2`,
`github.com/google/uuid`, `gopkg.in/yaml.v3`). Everything else is indirect.

## 3. Public surface per package

### internal/config
```
func IsLoopbackAddr(addr string) bool
type Account, Config, WebUI, Webhook
func Load(path string) (*Config, error)
func (*Config) Save(path string) error
func (*Config) Validate() error
```

### internal/googleauth
```
var Scopes = []string{calendar.CalendarEventsScope}
var ErrNeedsAuth
func Authorize(ctx, credentialsFile, tokenFile string) error
func Client(ctx, credentialsFile, tokenFile string, logger *slog.Logger) (*calendar.Service, error)
```

### internal/sync
```
var ErrNotOwned
type Account, Engine, Event, Ownership, TimeSpan, RetryPolicy
type CalendarClient interface   (Google-typed; what Engine consumes)
type Provider interface          (provider-neutral; forward-looking seam)
func NewGoogleCalendarClient(*calendar.Service) CalendarClient
func NewGoogleProvider(CalendarClient) Provider
func NewProviderClient(Provider, title string) CalendarClient
func NewRetryingClient(CalendarClient, RetryPolicy, *slog.Logger, account string) CalendarClient
func DefaultRetryPolicy() RetryPolicy
func (*Engine) SyncOnce(ctx) error
```

### internal/webhook
```
type Channel, Target, Manager, Receiver, Debouncer
type ChannelWatcher, Notifier interfaces
func NewGoogleWatcher(map[string]*calendar.Service) ChannelWatcher
func NewManager(...) *Manager;  func (*Manager) Run(ctx, []Target) error
func NewReceiver(token string, Notifier, *slog.Logger) *Receiver   (http.Handler)
func NewDebouncer(time.Duration) *Debouncer
```

### internal/webui
```
type Options, Server, Status, StatusFunc, SyncFunc
func New(Options) (*Server, error)
func (*Server) Handler() http.Handler
```

## 4. CLI surface

| Command | Flags | Behaviour | Exit codes |
|---|---|---|---|
| `auth` | `-config` (default `config.yaml`), `-account` (required) | Interactive OAuth2, writes token file | 0 ok; 1 missing `-account`, config load fail, unknown account, auth fail |
| `sync-once` | `-config` | One pass, then exit | 0 ok **and** on SIGINT/SIGTERM mid-pass; 1 setup fail or sync error |
| `run` | `-config` | Poll loop (+ optional push) until signal | 0 on signal; 1 setup/parse/webhook-bind fail |
| `ui` | `-config` | Serve loopback config UI | 0 on signal; 1 config load fail, `web_ui.enabled=false`, non-loopback bind, listen error |
| `help` / `-h` / `--help` | — | Usage to **stderr** | 0 |
| *(no args / unknown)* | — | Usage to stderr | 1 |

Absent: `-version`, `-dry-run`, `-json`, `-log-level`, `-log-format`.

## 5. Config keys: example file vs. loader

Every key in `config.example.yaml` **is** read by the loader, and every
loader key **is** represented in the example. No orphans in either direction.

| Key | Type | Default | Required | Validated |
|---|---|---|---|---|
| `accounts[].name` | string | — | yes | non-empty, unique |
| `accounts[].credentials_file` | path | — | yes | non-empty (existence not checked at load) |
| `accounts[].token_file` | path | — | yes | non-empty |
| `accounts[].calendar_id` | string | — | yes | non-empty |
| `poll_interval` | duration | `5m` | no | parses, `> 0` |
| `lookahead_days` | int | `30` | no | `>= 0` |
| `block_title` | string | `Busy (calendar-bridge)` | no | none |
| `webhook.enabled` | bool | `false` | no | — |
| `webhook.public_url` | url | — | if enabled | https, host set, no userinfo/query/fragment |
| `webhook.listen_addr` | host:port | `:8080` | no | none (bind fails at runtime) |
| `webhook.verification_token` | secret | — | if enabled | non-empty |
| `webhook.channel_ttl` | duration | `24h` | no | parses, `> 0` |
| `webhook.debounce_interval` | duration | `5s` | no | parses |
| `web_ui.enabled` | bool | `false` | no | — |
| `web_ui.listen_addr` | host:port | `127.0.0.1:8090` | no | loopback enforced in `webui.New` |
| `web_ui.auth_token` | secret | — | no | none |

Minimum-2-accounts is enforced in exactly one place (`Config.Validate`), reached
by both `Load` and `Save`.

**No environment-variable overrides exist**, despite the `internal/config`
package doc claiming otherwise.

## 6. Google API scopes

| Scope | Where | Needed? |
|---|---|---|
| `https://www.googleapis.com/auth/calendar.events` | `googleauth.Scopes` | Yes. The engine must read events and create/update/delete its own. No narrower scope permits event writes. |

Not requested (correctly): `calendar` (full, includes calendar list/ACL management),
`calendar.settings.readonly`, `calendar.readonly`.

The scope does grant read of event *titles, attendees, descriptions* — more than
the engine uses. That excess is structurally contained, not scope-contained: the
neutral `sync.Event` model carries no content fields at all.

## 7. Tests and coverage

```
cmd/calendar-bridge     0.0%
internal/config        85.3%
internal/googleauth    44.0%
internal/sync          76.0%
internal/webhook       67.3%
internal/webui         89.3%
```

108 test functions. Zero fuzz targets, zero benchmarks, zero `httptest`-based
Google API doubles (all fakes sit at the `CalendarClient` interface, above the
HTTP layer). No injected clock — `sync.SyncOnce` calls `time.Now()` directly and
fixtures are built as offsets from wall-clock now.

## 8. CI / release / container

**`.github/workflows/ci.yml`** — 3 jobs on `ubuntu-latest` only:
`test` (build, vet, `test -race -cover`, gofmt), `lint` (golangci-lint v2.13.1),
`vulncheck` (govulncheck v1.7.0). All actions pinned to SHAs.
`permissions: contents: read` at workflow level.
No concurrency group, no Go/OS matrix, no coverage gate, no coverage artifact.

**`.github/workflows/release.yml`** — on `v*` tags: goreleaser (`version: latest`,
unpinned) then a `docker` job building/pushing to GHCR. Per-job least-privilege
permissions. No SBOM, no cosign, no multi-arch image, no provenance attestation.

**`.golangci.yml`** — v2 config, enables `errcheck govet ineffassign staticcheck
unused gosec bodyclose noctx`. Not enabled: `revive`, `errorlint`, `contextcheck`,
`copyloopvar`, `misspell`, `godot`, `nilerr`, `perfsprint`, `testifylint`.

**`.goreleaser.yml`** — linux/darwin/windows × amd64/arm64, tar.gz/zip,
`checksums.txt`, changelog filters. Injects
`-X main.version -X main.commit -X main.date`.

**`Dockerfile`** — `golang:1.27-alpine` (tag, not digest) → `gcr.io/distroless/static-debian12:nonroot`.
`WORKDIR /app`, entrypoint `/app/calendar-bridge`, default CMD `run -config /app/config/config.yaml`.
No `HEALTHCHECK`, no build cache mounts.

**`fly.toml`** — one `shared-cpu-1x` / 256 MB machine, volume `cb_config` → `/app/config`,
no `[[services]]`. No `[deploy] strategy`, no auto-stop config.

**`.coderabbit.yaml`** — assertive profile; path instructions encode the two
critical invariants (owner tagging in `internal/sync`, credential handling in
`internal/googleauth`) plus Dockerfile-nonroot and fly.toml-no-services rules.

## 9. Docs vs. code

| Doc | Status |
|---|---|
| `README.md` | Mostly accurate. Docker and Fly.io instructions produce a **non-working deployment** (secret path mismatch, see audit F-01). One resolved item is still listed under "Known limitations". |
| `docs/web-ui.md` | Describes `#token=` URL-fragment auth as the mechanism; the code moved to `localStorage` with a one-shot fragment migration. Claims `/api/status` reports "last sync"; the wired `StatusFunc` only ever returns an account count. Omits that `webhook.verification_token` is also redacted. |
| `docs/webhooks.md` | Not contradicted by code (spot-checked against `internal/webhook`). |
| `CONTRIBUTING.md` | "Where things live" table omits `internal/webhook` and `internal/webui`; CLI row omits the `ui` subcommand. |
| `SECURITY.md` | Accurate. |
| `config.example.yaml` | Accurate. |
