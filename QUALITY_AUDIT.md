# calendar-bridge — quality audit

Audited at commit `85c4afc` (branch `main`), 2026-08-30.
Companion inventory: [`docs/INVENTORY.md`](docs/INVENTORY.md).

Every finding below was read out of the source. Line references are to the
audited commit.

---

## Executive summary

This is a well-built small Go project — genuinely above average for a
one-person side project. The ownership-tagging invariant is real, enforced at
several layers, and covered by tests that attack it adversarially. Errors are
wrapped, contexts are threaded, and shutdown is clean. Production wiring supplies
loggers, but an `Engine` whose `Logger` is left at its zero value (`nil`) can panic
when it processes an account (F-30). CI pins its actions by SHA, and the comments
explain *why* rather than *what*. The README's honesty about limitations is a rare
strength.

Three things limit it:

1. **The documented deployments do not work.** Both the Docker and Fly.io
   quickstarts mount secrets at a path the binary never looks in, and the
   distroless `nonroot` UID cannot read host-owned `0600` files. Anyone
   following the README hits "no such file or directory" on first run.
2. **The OAuth story has a reachable dead end.** `auth` requests offline access
   without forcing consent, so re-authorizing an already-granted account returns
   *no refresh token* — and re-running `auth` is exactly what the README tells
   you to do when a token expires. Combined with never re-persisting refreshed
   tokens, the documented recovery path can leave an account broken in an hour.
3. **The sync engine is correct about ownership but naive about calendars.**
   Events marked Free and invitations you declined still block time everywhere.
   All-day events blank out entire working days. Nothing is deduped across
   accounts. And `time.Now()` is called inline, so the lookahead boundary, the
   24h look-back, DST, and timezone-mismatch behaviour are all untestable and
   all untested.

None of this is data-destroying. The "never delete a real event" invariant
holds under every input I could construct.

---

## Scorecard

| Dimension | Score | Justification |
|---|:--:|---|
| Architecture & Go standards | 4/5 | Clean package graph, no cycles, interface-at-consumer, and wrapped errors. Loggers are injected rather than global, but an `Engine` whose `Logger` is left at its zero value (`nil`) can panic when it processes an account (F-30). Loses a point for a 370-line provider seam that production never touches. |
| Sync correctness | 3/5 | The ownership invariant is genuinely airtight. Calendar semantics are not: free/declined/all-day are unhandled, and the time-window edges are untested. |
| Security | 4/5 | Loopback enforcement, DNS-rebinding guard, CSRF, constant-time auth, atomic 0600 config writes, per-file permission warnings. Loses a point for the OAuth refresh gap and non-atomic token writes. |
| Testing & QA | 3/5 | 108 tests, adversarial where it counts, honest fakes. But no clock injection, no HTTP-level double, no fuzzing, no coverage gate, 0% on `cmd/`, and at least one tautological test padding the number. |
| Web UI & UX | 2/5 | Functional and safe, but unusable below ~700px, several contrast failures, nothing announced to screen readers, no confirmation on destructive actions, and the status panel is wired to a function that returns almost nothing. |
| CLI UX | 3/5 | Clear errors that name the fix. No `-version` (despite ldflags trying to inject one), no `-dry-run`, no `-json`, help goes to stderr. |
| CI/CD & supply chain | 3/5 | SHA-pinned actions, least-privilege permissions, govulncheck pinned — all good. No CodeQL, Scorecard, gitleaks, SBOM, signing, multi-arch image, matrix, or coverage gate. |
| Observability | 1/5 | Structured `slog` with reasonable fields, and that is the entire story. No metrics, no health endpoints, no stable event vocabulary. |
| Documentation | 3/5 | Well-written and unusually honest, but the deployment sections are broken and `docs/web-ui.md` describes behaviour the code no longer has. |
| Deployability | 2/5 | Dockerfile and fly.toml are sound artifacts; the instructions around them are not, and there are no systemd/launchd units or k8s manifests at all. |

---

## Findings

| ID | Dimension | Sev | Effort | Location | Description |
|---|---|:--:|:--:|---|---|
| F-01 | Deployability | **High** | S | `README.md:186-206`, `Dockerfile:11` | Documented Docker/Fly secret paths never resolve; container UID can't read them either. |
| F-02 | Security | **High** | S | `googleauth.go:89` | `AccessTypeOffline` without forced consent → re-auth yields no refresh token. |
| F-03 | Security | **High** | M | `googleauth.go:59` | Refreshed/rotated tokens are never written back to disk. |
| F-04 | Correctness | **High** | S | `sync.go:101-110` | Events marked Free, and declined invitations, still create Busy blocks. |
| F-05 | CLI/Release | **High** | S | `.goreleaser.yml:26` | `-X main.version/commit/date` target symbols that don't exist; no `-version` flag. |
| F-06 | Architecture | Medium | S | `main.go:141` | Provider seam is test-only; its delete-time ownership re-verification never runs in production. |
| F-07 | Testing | Medium | M | `sync.go:79` | No injected clock; all time-window behaviour untestable. |
| F-08 | Efficiency | Medium | M | `sync.go:197` | One `FindBlockBySource` API call per (event × target), despite the engine already holding every owned block in memory. |
| F-09 | Web UI | Medium | S | `main.go:390-401` | `Status` has 5 fields; the wired `StatusFunc` populates 1. Docs promise the rest. |
| F-10 | Observability | Medium | M | — | No `/metrics`, `/healthz`, `/readyz`. |
| F-11 | Web UI / a11y | Medium | M | `index.html:20,24,29,39,169` | Not responsive, contrast failures, no `aria-live`, unconfirmed destructive action. |
| F-12 | Web UI | Medium | S | `index.html:228,246` | Field-level validation message is swallowed by the outer catch. |
| F-13 | Security | Medium | S | `googleauth.go:164` | Token file write is not atomic, unlike `config.Save`. |
| F-14 | Correctness | Medium | M | `sync.go:129-138` | No cross-account dedup; a meeting you're invited to on two accounts gets a redundant block on each. |
| F-15 | Correctness | Medium | S | `sync.go:216-218` | All-day events become all-day opaque blocks, blanking whole working days. |
| F-16 | CI/CD | Medium | S | `ci.yml` | No concurrency group, no coverage gate, no Go/OS matrix. |
| F-17 | Supply chain | Medium | M | `release.yml:30,58` | Unpinned goreleaser-action; no SBOM, signing, provenance, or multi-arch image. |
| F-18 | Security | Low | S | `receiver.go:88` | Channel-token compare leaks length; `webui` already does this correctly. |
| F-19 | Docs | Low | S | `config.go:2` | Package doc claims env-var overrides that don't exist. |
| F-20 | Security | Low | S | `handlers.go:35` | Config path leaked to the browser in an error string. |
| F-21 | Security | Low | S | `handlers.go:144-148` | No `Referrer-Policy`; no `Cache-Control: no-store` on `/api/config`. |
| F-22 | CLI UX | Low | S | `main.go:51-61` | `--help` writes to stderr. |
| F-23 | Docs | Low | S | `server.go:57` | Doc comment references a `Serve` method that doesn't exist. |
| F-24 | CLI UX | Low | S | `googleauth.go:54-57` | A corrupt token file reports "not yet authorized", hiding the real cause. |
| F-25 | Container | Low | S | `Dockerfile:3,10` | Base images pinned by tag, not digest; no `HEALTHCHECK`. |
| F-26 | Correctness | Low | S | `retrying_client.go:140` | Conditional delete retried with a stale ETag reports a failure for a delete that succeeded. |
| F-27 | Correctness | Nit | S | `sync.go:204-210` | `block_title` changes never propagate to existing blocks. |
| F-28 | Correctness | Nit | S | `sync.go:197` | A user-duplicated block is half-managed and eventually GC'd. Undocumented. |
| F-29 | Go standards | Nit | S | `sync.go:191` | `deterministicBlockKey` is dead code, kept alive by a tautological test. |
| F-30 | Go standards | Nit | S | `sync.go:95` | `Engine.Logger` has no nil guard; an otherwise configured `Engine` panics while processing an account if `Logger` is left nil. |

---

## Detailed findings

### High

#### F-01 — The documented Docker and Fly.io deployments cannot work

`Dockerfile:11` sets `WORKDIR /app`. `config.example.yaml:8` uses relative
secret paths (`secrets/personal-credentials.json`), and the README never says
to change them. `README.md:204-206` mounts the host `secrets/` directory at
`/app/config/secrets`:

```bash
docker run -v $(pwd)/config.yaml:/app/config/config.yaml:ro \
           -v $(pwd)/secrets:/app/config/secrets:ro \
           calendar-bridge
```

The binary resolves `secrets/personal-credentials.json` against its working
directory, `/app` — so it looks in `/app/secrets`, which is empty. First run
fails with `reading credentials file secrets/personal-credentials.json: no such
file or directory`. The Fly.io instructions (`README.md:186-191`) upload to the
same wrong location and fail identically.

There is a second, independent failure underneath it: the runtime base is
`gcr.io/distroless/static-debian12:nonroot`, which runs as UID 65532. Token and
credentials files are `0600` and owned by the host user, so even at the correct
path they are unreadable.

**Fix.** Document absolute paths in the container (`/app/config/secrets/...`),
ship a working `docker-compose.yml` that sets `user:` to match the mounted
files' owner, and add a "you should now see…" verification step. Ideally also
support a `CB_SECRETS_DIR` prefix so one config works both locally and in a
container.

#### F-02 — Re-authorizing an account silently produces a token with no refresh token

`googleauth.go:89`:

```go
authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
```

Google issues a refresh token for an installed-app flow **only on the first
authorization** of a given client/user pair. Every subsequent authorization
returns an access token alone unless the request carries
`prompt=consent` (`oauth2.ApprovalForce`). `saveToken` then persists a token
whose `RefreshToken` is empty, and the account stops working roughly an hour
later with a non-retryable 401.

This is reachable through the project's own documented remedy:
`README.md:267` tells users to "re-run `auth` for any account whose token
expired".

**Fix.** `config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)`,
and have `saveToken` refuse to overwrite an existing token that has a refresh
token with one that doesn't.

Two adjacent issues in the same call, worth fixing together:
- The `state` parameter is the literal string `"state-token"` and is never
  verified. Use a per-run random value and check it against the pasted redirect
  URL when one is pasted.
- No PKCE. `golang.org/x/oauth2` supports it directly
  (`oauth2.GenerateVerifier()` + `S256ChallengeOption`/`VerifierOption`), and
  Google recommends it for installed apps.

> Touches `internal/googleauth`. Proposed, not applied — awaiting your sign-off.

#### F-03 — Refreshed tokens are never persisted

`googleauth.go:59` hands the stored token to `config.Client(ctx, tok)`, which
refreshes in memory. Nothing writes the refreshed token back.

Consequences, in increasing severity:
- Every process start burns a refresh round-trip before the first API call.
- If the OAuth consent screen is in **Testing** publishing status, Google
  expires refresh tokens after 7 days. The user must re-auth — and F-02 means
  that re-auth produces a token that dies in an hour.
- If Google ever rotates the refresh token, the on-disk copy is stale
  immediately.

The README discloses this (line 234) and rates it "low-risk in practice". That
was fair in isolation; chained with F-02 it isn't.

**Fix.** Wrap the token source and persist on change:

```go
ts := config.TokenSource(ctx, tok)
persisting := &persistingTokenSource{inner: ts, path: tokenFile, last: tok}
httpClient := oauth2.NewClient(ctx, persisting)
```

where `persistingTokenSource.Token()` re-saves (atomically, per F-13) whenever
the returned access or refresh token differs from the last one seen.

> Touches `internal/googleauth`. Proposed, not applied — awaiting your sign-off.

#### F-04 — Free-marked and declined events still block time everywhere

`sync.go:101-110` classifies a fetched event using exactly two signals: is it
cancelled, and is it owner-tagged. Everything else lands in `real` and gets a
Busy block on every other account.

So:
- An event you set to **Free** ("show as available") in Google Calendar —
  `transparency: "transparent"` — produces an *opaque* block elsewhere
  (`sync.go:218` hardcodes `Transparency: "opaque"`).
- A meeting you **declined** still holds the slot on every other calendar.
- A **tentative** invitation is treated as firmly busy.

Declining a meeting and still losing the slot on three other calendars is the
failure users will actually report, and it looks like the tool is broken rather
than under-specified.

The neutral `sync.Event` model can't express any of this either — it carries
`Cancelled` and nothing else (`provider.go:89-102`) — so the fix touches the
Provider seam as well.

**Fix.** Skip events where `Transparency == "transparent"`, and where the
`Attendees` entry with `Self == true` has `ResponseStatus == "declined"`. Make
`tentative` handling a config key (`treat_tentative_as_busy`, default true).
Add `Transparent bool` and `SelfResponse string` to `sync.Event`.

> Touches sync invariants. Proposed, not applied — awaiting your sign-off.

#### F-05 — Released binaries have no version

`.goreleaser.yml:26` injects:

```text
-X main.version={{.Version}} -X main.commit={{.Commit}} -X main.date={{.Date}}
```

`package main` declares none of those variables (`grep` across `cmd/` returns
nothing), so the linker silently drops all three. There is also no `-version`
subcommand or flag, so a user with a downloaded binary has no way to say which
build they're running — which makes every future bug report worse.

**Fix.** Declare `var version, commit, date = "dev", "none", "unknown"` in
`main`, add a `version` subcommand and a `-version` flag, and print Go version
and `runtime/debug.ReadBuildInfo()` VCS data as a fallback for `go install`
builds.

### Medium

#### F-06 — The Provider seam is not in the production path

`main.go:141-146` builds each account's client as:

```go
sync.NewRetryingClient(sync.NewGoogleCalendarClient(svc), ...)
```

Not `NewProviderClient(NewGoogleProvider(...))`. So `googleProvider` and
`providerClient` — about 370 lines including the delete-time ownership
re-verification loop with 412 handling (`google_provider.go:213-246`), the
insert idempotency check (`:126-130`), and the update full-replace preservation
(`:167-211`) — execute only under test.

Production deletion safety therefore rests entirely on the `If-Match` ETag from
the list (`sync.go:168`), with no re-read and no `isOwnedBlock` re-check. That
is *probably* sufficient, because Calendar API honours `If-Match` on delete and
any tampering changes the ETag. But it means the layer written specifically to
guarantee this doesn't run, and a reader of `google_provider.go` would
reasonably conclude otherwise.

Note the insert idempotency *is* covered in production — `retryingClient`
carries its own reconciliation (`retrying_client.go:104-125`). Only the delete
and update hardening is bypassed.

**Options.** (a) Wire the bridge in `buildEngine` and get the hardening for
real; (b) leave it and add a prominent comment in `provider.go` saying it is a
future seam, plus move the delete re-verification down into
`googleCalendarClient`. I recommend (a), but it changes the production write
path.

> Touches sync invariants. Proposed, not applied — awaiting your sign-off.

#### F-07 — No injected clock

`sync.go:79` calls `time.Now()` directly; `fake_client_test.go:194` builds every
fixture as an offset from wall-clock now. Nothing can assert on:

- an event exactly at the `timeMax` lookahead boundary, or crossing it in
  either direction;
- the 24h look-back buffer, or an in-progress event at its edge;
- a DST transition inside an event's span;
- two accounts whose events are expressed in different IANA zones;
- clock skew between the host and Google.

**Fix.** Add `Now func() time.Time` to `Engine` (defaulting to `time.Now`), a
tiny `clock` helper for tests, and rebuild the fixtures around a fixed base
time.

#### F-08 — One API call per (event × target account), for data already in memory

Step 1 of `SyncOnce` lists every account's events and separates the owned blocks
into `byAccount[name].owned` (`sync.go:100-113`). Step 2 then throws that away:
`ensureBlock` calls `dst.Client.FindBlockBySource(...)` (`sync.go:197`) — a
network round trip — for every (source event, target account) pair.

For 3 accounts with 50 events each, that is 300 `events.list` calls per pass,
issued serially, on top of the 3 needed. At 5 accounts × 200 events it is 4,000
serial calls, which will not finish inside `syncCycleTimeout` (5 minutes).

The lookup can be a map built once from the data already fetched:
`map[srcAccount+"|"+srcEventID]*calendar.Event` per destination account.
`FindBlockBySource` then becomes a fallback for blocks outside the listed window
rather than the primary path.

Secondary: the whole pass is serial. Even after the map fix, writes for N
accounts should run under a bounded `errgroup`.

#### F-09 — The status panel is wired to a stub

`webui.Status` declares `Running`, `LastSync`, `LastError`, `AccountsNum`,
`PushEnabled` (`server.go:47-54`), and `index.html:193-206` renders last-sync
and last-error. But `main.go:400` returns:

```go
return webui.Status{AccountsNum: len(current.Accounts)}
```

`Running` and `PushEnabled` are never set anywhere in the repo. So the UI
permanently reads "N accounts · not synced yet", even immediately after a
successful manual sync. `docs/web-ui.md` advertises "(account count, last sync)".

**Fix.** Keep a small `syncState` struct in `runUI` (mutex-guarded: last start,
last success, last error, last pass counts), update it in `syncNow`, and return
it from `statusFn`. Extend `Status` with per-account health and
created/updated/deleted counts, which requires `SyncOnce` to return a result
struct rather than only an error.

#### F-10 — No metrics or health endpoints

The roadmap's last open item. Nothing exposes pass duration, blocks
created/updated/deleted, per-account error counts, or a last-success timestamp;
and there is no `/healthz` for a k8s probe or a Docker `HEALTHCHECK` to target.
Adding `/metrics` means a new dependency (`prometheus/client_golang`) — I'd
rather ask than assume. A hand-rolled text-format exposition is possible with
zero deps if you'd prefer.

#### F-11 — Web UI accessibility and responsiveness

- `index.html:24` — `.row { grid-template-columns: 1fr 1fr 1fr 1fr auto }` with
  no media query. Four text inputs plus a button across a 380px viewport gives
  each field ~70px. Unusable on a phone and bad at 200% zoom.
- `index.html:29` — `.primary { background: #2a7; color: #fff }` is roughly
  2.3:1. WCAG AA needs 4.5:1.
- `index.html:20,39` — `label { color: #888e }` and `.muted { color: #8888 }`
  are translucent mid-greys that fail contrast against both light and dark
  backgrounds.
- `index.html:51,100` — neither `#status` nor `#toast` has `aria-live`, so a
  screen-reader user gets no announcement of a save result or a sync failure.
- `index.html:169` — "Remove" deletes an account row immediately, no
  confirmation, no undo.
- `index.html:170` — an empty `<label>` element used purely for grid alignment.

Also worth noting: there is no empty state for a fresh config, no loading state
on the form, and no distinct rendering for the two failures users will actually
hit (account not authorized, token expired) — the UI has no way to know about
either, because of F-09.

#### F-12 — Validation errors are swallowed

`collectConfig()` throws `new Error("lookahead_days must be a non-negative
integer")` (`index.html:228`). The submit handler's `catch` (`:246`) turns any
throw into `showToast("Save failed.", false)`. The precise, actionable message
is discarded and the user sees nothing about which field is wrong. Form input is
preserved (nothing re-renders), which is good.

**Fix.** Catch the validation error separately and surface `e.message`, ideally
with an inline `aria-describedby` error on the offending input.

#### F-13 — Token file writes are not atomic

`googleauth.go:164` opens with `O_TRUNC` and encodes in place. A crash, a full
disk, or a killed container between truncate and flush leaves a zero-length or
half-written token file; `tokenFromFile` then fails and the account reports as
never authorized.

`config.Save` (`config.go:187-229`) does this correctly — temp file, chmod 0600,
write, fsync, rename. The token file matters more and gets less care.

**Fix.** Extract the temp-write-rename helper and use it for both.

#### F-14 — No cross-account deduplication

If the same meeting appears as a real event on two configured accounts — which
is the normal case when you're invited on both your personal and work addresses
— account A creates a block on B and B creates a block on A. Each calendar ends
up showing the real event *and* a redundant "Busy (calendar-bridge)" block
stacked on the same slot. Forever.

Google exposes `iCalUID` for exactly this. `sync.Event` doesn't carry it.

**Fix.** Add `ICalUID` to the neutral model; before creating a block on `dst`,
skip if any real event on `dst` shares the source's `iCalUID`. Requires care:
the block must still be created if the user later deletes their copy.

> Touches sync invariants. Proposed, not applied — awaiting your sign-off.

#### F-15 — All-day events blank out whole days

`ensureBlock` copies `srcEvent.Start`/`End` verbatim (`sync.go:216-217`) and
stamps `Transparency: "opaque"` (`:218`). An all-day source event has `Date` set
rather than `DateTime`, so the block is an all-day event too — and marked Busy.

A birthday, a public holiday, or a multi-day PTO entry on your personal calendar
therefore renders every working hour of those days as Busy on every Workspace
calendar. Most all-day events in Google Calendar are `transparent` by default,
which F-04's fix would filter — but an all-day event explicitly marked Busy
would still land, and there's no test for either path.

**Fix.** Decide the policy explicitly and make it a config key
(`all_day: skip | busy | working-hours`), defaulting to `skip`. Test it.

#### F-16 — CI gaps

`ci.yml` has no `concurrency:` group, so pushing three commits in a row runs
three full matrices of jobs and lets a stale one report last. It runs
`go test ./... -race -cover` and discards the coverage number. It runs on
`ubuntu-latest` only, while `.goreleaser.yml` ships darwin and windows binaries
and the docs recommend a launchd unit.

**Fix.** Add a concurrency group with `cancel-in-progress`, a Go version matrix
(`stable` + `oldstable`), an OS matrix for at least `macos-latest`, and a
coverage gate. Achievable threshold today, honestly: **70% overall** with a
per-package floor of **75% for `internal/sync`** — both already met, so the gate
locks in the status quo rather than pretending to raise it. Raise after the
Phase-3 test work lands.

#### F-17 — Release supply chain gaps

- `release.yml:30` — `goreleaser-action` with `version: latest`. An unpinned
  tool in the artifact-producing path, in a repo that otherwise pins every
  action by SHA and even pins `govulncheck` with a comment explaining why.
- No SBOM. goreleaser has first-class syft support (`sboms:`).
- No signatures. No cosign keyless signing of archives, checksums, or image.
- `release.yml:58` — `docker/build-push-action` with no `platforms:`, so the
  GHCR image is amd64-only. arm64 *binaries* ship, but the Raspberry Pi and
  Apple-silicon container stories are broken.
- No provenance attestation (`actions/attest-build-provenance`).
- Missing from the repo entirely: CodeQL, OpenSSF Scorecard, gitleaks, a
  markdown link checker, PR title lint.

### Low and Nit

**F-18** `receiver.go:88` — `subtle.ConstantTimeCompare([]byte(got), []byte(r.token))`
returns 0 immediately when lengths differ, so response timing leaks the
verification token's length. `webui.validToken` (`server.go:226-228`) already
hashes both sides to fixed width first. Make the webhook receiver match.

**F-19** `config.go:2` — "loads calendar-bridge configuration from a YAML file
and environment variable overrides". There is no `os.Getenv` or `os.LookupEnv`
anywhere in the repository. Either implement the overrides (useful for
containers: `CB_POLL_INTERVAL`, `CB_LOOKAHEAD_DAYS`) or delete the claim.

**F-20** `handlers.go:35` — `"loading config: "+err.Error()`. `config.Load`
embeds the config path in its error (`config.go:136`), so the path is sent to
the browser. `main.go:357-360` deliberately suppresses exactly this in logs.
Inconsistent; loopback-only, so impact is low.

**F-21** `handlers.go:144-148` — the index sets CSP and `nosniff` but no
`Referrer-Policy: no-referrer`. `/api/config` returns account metadata and
paths with no `Cache-Control: no-store`.

**F-22** `main.go:51-61` — explicit `help`/`-h`/`--help` writes usage to
**stderr** and exits 0. `calendar-bridge --help | less` shows an empty pager.
Help to stdout on success; usage to stderr only on error.

**F-23** `server.go:57` — "Construct with New and mount Handler() or call
Serve." There is no `Serve` method on `Server`.

**F-24** `googleauth.go:54-57` — the decode error from `tokenFromFile` is
discarded and replaced with `ErrNeedsAuth`. A corrupt or truncated token file
(see F-13) reports as "account not yet authorized", sending the user down the
wrong path.

**F-25** `Dockerfile:3,10` — `golang:1.27-alpine` and
`distroless/static-debian12:nonroot` are floating tags. Pin by digest and let
Dependabot bump them (the docker ecosystem is already configured). No
`HEALTHCHECK` — nothing to point one at until F-10 lands.

**F-26** `retrying_client.go:140-143` — `DeleteEvent` retries with the same
`ifMatchETag`. If the first delete succeeded but the response was lost, the
retry gets 404/410 and the pass reports a spurious deletion failure. Treat
"not found" as success on a retried delete.

**F-27** `sync.go:204-210` — the update path compares only `Start` and `End`. If
you change `block_title` in config, every existing block keeps the old title
indefinitely; only new blocks get the new one.

**F-28** `sync.go:197` — Google's "Duplicate event" copies private extended
properties. A user who duplicates a Busy block gets two events with identical
source identity: `FindBlockBySource` returns whichever the API lists first, so
only one is ever time-updated, and GC removes both when the source disappears.
Not dangerous, but undocumented and untested.

**F-29** `sync.go:191` — `deterministicBlockKey` is called from nowhere except
`sync_test.go:117-127`, which asserts only that a pure function is
deterministic. Dead code plus a test that can never fail.

**F-30** `sync.go:95` — `Engine.Logger` is dereferenced with no nil check, and
unlike `NewRetryingClient` (`retrying_client.go:44-46`) nothing defaults it. An
otherwise configured `Engine` with a nil `Logger` panics while processing its
first account.

---

## Invariant coverage matrix

| # | Invariant | Enforced? | Tested? | Gap |
|---|---|---|---|---|
| 1 | A block is never mistaken for a real event | Yes — `isOwnedBlock` (`sync.go:47`) is the single source of truth; re-verified in `FindBlockBySource` (`client.go:104`), `googleProvider.FindOwnedBlock` (`:107`), and `DeleteBlock` (`:231`) | Yes — `TestIsOwnedBlock`, `TestSyncOnce_NeverOverwritesUnownedEventWithMatchingProperties`, `TestGoogleProvider_IgnoresSourceMatchButUntagged`, `TestGoogleProvider_FindOwnedBlockNeverReturnsUnowned` | **User-duplicated block** (F-28) untested. Note: an external organizer cannot set `extendedProperties.private` on *your* copy, so the adversarial path is closed — worth stating in the threat model. |
| 2 | No sync loops — a block never propagates onward | Yes — owned blocks go to `ae.owned`, never `ae.real` (`sync.go:105-109`) | **No** | No three-account test proving a block on B is not re-propagated to C. The single most important invariant has no direct test. |
| 3 | A real human event is never deleted | Yes — GC iterates only `owned` (`sync.go:151`), delete is ETag-conditional (`:168`), and the Provider path re-reads and re-checks | Partly — `TestDeleteBlock_RefusesUntaggedUnderETag`, `TestDeleteBlock_On412BecomesUntaggedRefuses`, `TestGoogleProvider_DeleteRejectsUnownedTarget` all exercise the **Provider** path, which production doesn't use (F-06) | No test that the *production* wiring refuses to delete an untagged event. No malformed/partial-data fuzzing. |
| 4 | GC only deletes blocks whose source is provably gone | Yes — `accountIsHealthy` guard (`sync.go:160`) | Yes — `TestSyncOnce_FailedSourceAccountDoesNotTriggerGC`, `TestSyncOnce_FailedAccountExcludedButOthersStillSync` | This one is genuinely well covered. |
| 5a | Moved event → block moves | Yes (`sync.go:204-209`) | Yes — `TestSyncOnce_UpdatesBlockWhenSourceEventMoves` | — |
| 5b | Shortened event → block shortens | Yes (same path) | **No** | Same code path as 5a; a table case would close it cheaply. |
| 5c | Cancelled event → block removed | Yes (`sync.go:102`) | Yes — `TestSyncOnce_GarbageCollectsRemovedSourceEvent` | Only the *deleted* case; no test seeding `status: "cancelled"`. |
| 5d | Declined invitation → no block | **No** (F-04) | **No** | Not implemented. |
| 5e | Tentative invitation | **No** — treated as busy, undocumented | **No** | Not implemented, not decided. |
| 5f | Free/transparent event → no block | **No** (F-04) | **No** | Not implemented. |
| 5g | All-day event | Propagates as all-day opaque (F-15) | **No** | Behaviour is accidental, not chosen. |
| 5h | Recurring event instances | Yes via `SingleEvents(true)` (`client.go:67`) — each instance is a distinct ID with its own block | **No** | No test. Recurrence exceptions (a single moved instance) and cancelled instances are the classic bug farm here and are entirely uncovered. The fake doesn't model recurrence at all. |
| 6 | Events crossing the lookahead boundary | Window is `[now-24h, now+lookahead)` (`sync.go:80-81`) | **No** | Blocked by F-07 (no injected clock). Nothing tests entering or leaving the window. |
| 7 | Clock skew / 24h look-back buffer | Buffer exists (`sync.go:80`) | **No** | Blocked by F-07. |
| 8 | Timezone mismatch between accounts | Start/End strings copied verbatim including `TimeZone` (`sync.go:216-217`); `TimeSpan.Equal` is a string compare, documented as deliberate (`provider.go:45-54`) | **No** | No test with two accounts in different zones. The string-compare choice is sound *because* values round-trip byte-identically — but nothing proves Google returns them unchanged. |
| 9 | Idempotency — second pass writes nothing | Yes by construction (`sync.go:210` returns early when times match) | **No** | No test runs `SyncOnce` twice and asserts zero writes on the second. Straightforward to add with a counting fake. |
| 10 | Partial-pass safety — a failed account never causes writes on its behalf | Yes (`sync.go:120-123`, `:160`) | Yes — two integration tests | Well covered. |
| 11 | Insert idempotency under ambiguous failure | Yes in production (`retrying_client.go:104-125`) and in the Provider (`google_provider.go:126-130`) | Yes — `TestRetryingClient_InsertEvent_ReconcilesAmbiguousResult`, `TestInsertBlock_IdempotentReusesExisting` | Well covered. |
| 12 | Event content never crosses accounts | Yes, structurally — the block is built from `Summary: e.BlockTitle` and times only (`sync.go:214-228`), and neutral `Event` has no content fields | Implicitly | No explicit test asserting a created block carries none of the source's summary/description/attendees. Cheap to add and it guards the project's headline privacy claim. |

**Summary: 6 of 19 rows are fully covered, 2 are partially covered, and 11 have
no coverage. “Partly” and “Implicitly” are both classified as partial coverage.**
The gaps cluster in exactly two places: calendar semantics (5d–5h) and anything
requiring control of time (6, 7, and by extension 9).

---

## Prioritized plan

### Fix now, this session (no invariant or OAuth changes)

| ID | Why now |
|---|---|
| F-01 | The quickstart is broken. Nothing else matters if first run fails. |
| F-05 | Trivial, and every future bug report is worse without it. |
| F-09 | The UI's main panel is a stub; fixing it is local to `main.go` + `webui`. |
| F-11, F-12 | Self-contained in one HTML file. |
| F-13 | Small, and it makes F-03's fix safe when you approve it. |
| F-16 | Cheap CI hardening; the coverage gate locks in current numbers. |
| F-18–F-30 | All small, all local, no behaviour change. |
| — | Test infrastructure: injected clock (F-07), counting fake, `httptest` Google double, `Makefile` with `ci` target. |
| — | Invariant-matrix gaps that need no behaviour change: rows 2, 3 (production wiring), 5b, 5c, 5h, 6, 7, 9, 12. |

### Next PR — needs your sign-off first

| ID | Decision needed |
|---|---|
| F-02, F-03 | OAuth changes. Do you want forced consent + PKCE + token persistence? I recommend all three. |
| F-04 | Should Free/declined events be skipped? I think obviously yes. Tentative is a genuine judgment call. |
| F-15 | What should an all-day Busy event do? I'd default to skipping it. |
| F-06 | Wire the Provider bridge into production, or demote it to a documented future seam? |
| F-08 | The in-memory block index is a clear win; the `errgroup` parallelism is a bigger change. |
| F-10 | Metrics need a new dependency (`prometheus/client_golang`) unless you want a hand-rolled exposition. Your call. |

### Issue, later

F-14 (cross-account dedup — needs design), F-17 (SBOM/cosign/multi-arch —
mechanical but touches the release path, best done as its own PR with a test
tag), plus the Phase-8 backlog: docs site, Homebrew tap, Helm chart, a real
CalDAV provider to prove the seam, encrypted-at-rest tokens, `--dry-run` diff
mode, and an `uninstall` command that removes every block the tool ever created.
