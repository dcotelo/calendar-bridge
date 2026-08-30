# Backlog

What calendar-bridge needs to be genuinely good, beyond the audit findings.
Each entry has value, effort, risk, and a recommendation.

Effort: **S** ≈ a day, **M** ≈ a few days, **L** ≈ a week or more.

---

## Recommended: do these

### 1. `calendar-bridge uninstall` — remove every block it ever created

| | |
|---|---|
| Value | **High.** The single worst thing about the tool today. Every deployment guide ends with "now search each calendar by hand and delete the results." Garbage collection cannot do it — it only removes blocks whose source is provably gone, and it only sees the fetch window. |
| Effort | **M.** A wide `events.list` per account with the owner property filter and no time bound, paginated, then a checked delete each. Needs `--dry-run` and a confirmation. |
| Risk | **Medium, and it is deletion.** But the blast radius is bounded by the same ownership check every other delete uses, and it is strictly narrower than what the engine already does: no source-matching, just "is it ours". A dry run listing exactly what would go, plus a typed confirmation, makes it safe. |

The one caveat worth designing around: an unbounded window means a genuinely
large result set on an old calendar. Paginate and report progress.

**Roadmap. Highest value item here.**

### 2. Encrypted token storage

| | |
|---|---|
| Value | **High.** The largest gap in the threat model. Tokens are plaintext on disk; anyone with the filesystem has your calendars. It is also the thing that stops "put it on a shared box" being reasonable. |
| Effort | **M** for one backend, **L** for three. macOS Keychain via `security`, systemd credentials on Linux, `age` with a passphrase as the portable fallback. |
| Risk | **Medium.** Getting it wrong locks people out of their own tokens. Needs a migration path both ways and must degrade to plaintext with a warning rather than refusing to start. |

Start with `age`: portable, no platform code, and the recovery story is a file
and a passphrase rather than an OS keystore. Keychain second.

**Roadmap.**

### 3. Cross-account deduplication by `iCalUID`

| | |
|---|---|
| Value | **Medium-high.** Being invited to the same meeting on two configured accounts puts a real event *and* a redundant block on the same slot, on both calendars, forever. Common for anyone with a personal and a work address on the same invitation. |
| Effort | **M.** Add `ICalUID` to the neutral model; skip creating a block on a target that already has a real event with the same UID. |
| Risk | **Medium.** The subtlety is the reverse transition: if the user later deletes their copy on the target, the block must then appear. That means the decision has to be re-evaluated every pass, not cached — and it interacts with the "excluded account" rule, since a missing UID could mean "deleted" or "failed to fetch". |

**Roadmap, but design it properly first.** The failure mode of getting it wrong
is a missing block, which is exactly the bug the tool exists to prevent.

### 4. A Homebrew tap

| | |
|---|---|
| Value | **Medium.** `brew install dcotelo/tap/calendar-bridge` is a materially lower barrier than `go install` for a Mac user, and macOS is the most likely laptop deployment. |
| Effort | **S.** goreleaser has first-class `brews:` support; it needs a `homebrew-tap` repo and a token. |
| Risk | **Low.** |

**Roadmap. Best value-per-hour on this list.**

### 5. A documentation site

| | |
|---|---|
| Value | **Medium.** There are now ~4,000 lines of docs across 20 files. GitHub renders them but does not make them navigable or searchable. You already do this for `ctf-in-a-box`, so the pattern and the styling exist. |
| Effort | **M.** GitHub Pages, matching `dcotelo.dev`. The content is already written and cross-linked. |
| Risk | **Low.** Main risk is drift between the site and the repo; publish from `docs/` on `main` so there is one source. |

**Roadmap.**

### 6. A `-diff` mode for dry runs

| | |
|---|---|
| Value | **Medium.** `-dry-run` currently reports counts. Before an upgrade that changes which blocks exist — the Free/declined change removed blocks for a lot of people — what you actually want is *which* blocks, with times. |
| Effort | **S.** The information is already computed; it is not printed. |
| Risk | **Low**, but note it means printing block times to stdout. Times only, never source event titles. |

**Roadmap. Cheap, and it makes every future behaviour change safer to adopt.**

### 7. A CalDAV or Outlook provider

| | |
|---|---|
| Value | **Medium.** Proves the `Provider` seam is real rather than aspirational, and opens the tool to people whose second calendar is not Google. Fastmail and iCloud via CalDAV are the likely first asks. |
| Effort | **L.** CalDAV is a genuinely awkward protocol, and the ownership tag has no direct equivalent — it would need `X-` properties in the VEVENT, with the per-copy privacy question re-answered from scratch. |
| Risk | **High for the invariants.** [ADR 0002](adr/0002-extended-properties-ownership.md) rests on Google's private extended properties being per-copy and unforgeable by an organiser. If a CalDAV server does not offer that, the ownership guarantee is weaker there and must be documented as such, not papered over. |

**Roadmap, but only after 1 and 2.** And write the ownership analysis before
writing any code.

---

## Worth doing, lower priority

### 8. A Helm chart

Value **low-medium** — the kustomize manifests already work and are simpler to
read. Effort **S-M**. Risk low. Worth it only if people actually ask.
**Issue, not roadmap.**

### 9. Per-account `lookahead_days` and `block_title`

Value **low-medium**: a work calendar might want 90 days while personal wants
14. Effort **S**. Risk low. **Issue.**

### 10. Configurable all-day handling

Value **medium** for the people it affects. The F-04 fix already handles most
cases, since all-day events default to Free. Effort **S**
(`all_day: skip | busy`). Risk low. **Issue.**

### 11. End-to-end tests against a real test Workspace

Value **medium-high** — it is the one thing the current suite cannot do, and
Google's actual behaviour around recurrence exceptions and extended-property
persistence is exactly where remaining bugs would hide. Effort **L**, and it
needs a dedicated Workspace and secrets in CI. Risk: **a scheduled job holding
live calendar credentials**, which is a new and permanent attack surface on this
repo.

**Issue, and think hard before doing it.** A manually-run harness against your
own test accounts, documented in [QA.md](QA.md), gets most of the value with
none of the credential risk.

---

## No, and here is why

### Multi-user support

**No.** It changes what the project is. Today one process serves one person, and
that is why there is no database, no authentication model beyond a loopback
bind, and no tenancy anywhere. Multi-user means user accounts, per-user token
isolation, an authorization model, and a real web application — and at that
point you have rebuilt the hosted service the tool exists as an alternative to.

Anyone who wants this should run one instance per person. It is a single static
binary; that is cheap.

### A telemetry-free update check

**No.** "Telemetry-free" is doing a lot of work in that phrase. An update check
is an outbound request, on a schedule, from a machine running the tool — which
tells the endpoint that a given IP runs calendar-bridge and roughly when. That
is telemetry, however it is framed, and the headline claim of this project is
that **nothing leaves your infrastructure except Calendar API calls**. Undermining
that to save people a `go install` is a bad trade.

Watch the repo, or subscribe to releases. Both are pull, not push.

### i18n of the web UI

**No, for now.** The UI is one page that one person looks at occasionally, and
the CLI, the logs, the config keys and the entire documentation set are English.
Translating the page alone would be a token gesture; translating everything is a
large, permanent maintenance commitment for a single-maintainer project.

Reconsider if someone actually asks and offers to maintain a locale.

### A hosted version

**No.** It is the thing this project exists as an alternative to. Running it
would mean holding other people's calendar tokens, which is precisely the
property that makes hosted tools unattractive to the audience for this one.

### A GUI installer / menu-bar app

**No.** Real work for a narrow audience, and it would need code signing,
notarization and an update mechanism — the last of which is the update-check
problem above, plus an auto-updater. The Homebrew tap covers most of the
convenience at a fraction of the cost.

### Syncing event content behind a flag

**No.** See [ADR 0003](adr/0003-no-event-content-propagation.md). It converts a
structural guarantee into a runtime boolean, and the failure mode of getting
that boolean wrong is disclosing a client meeting to a different employer.

---

## Repo hygiene the maintainer has to do

These cannot be done from a pull request.

- [ ] Set the repository **description** and **homepage** (currently empty).
- [ ] Set **topics**: `google-calendar`, `calendar-sync`, `self-hosted`, `golang`, `privacy`, `oauth2`, `busy-time`.
- [ ] Enable **branch protection** on `main` — the recommended settings are in [CONTRIBUTING.md](../CONTRIBUTING.md#recommended-branch-protection).
- [ ] Enable **Dependabot alerts** and **secret scanning** in repository settings.
- [ ] Enable **GitHub Pages** if the docs site goes ahead.
- [ ] Create the `homebrew-tap` repository if the tap goes ahead.
- [ ] Cut a first tagged release, so `pkg.go.dev`, the release badge and the GHCR package page resolve.
