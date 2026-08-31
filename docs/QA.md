# Manual QA checklist

Automated tests cover the sync invariants, the HTTP handlers, and the config
loader. They cannot cover the things that need a real Google account, a real
browser, or a real machine restart. This is the list a human walks before
tagging a release.

Run it against a **throwaway pair of Google accounts**, never your real ones.

```bash
make ci        # everything automated must pass first
```

Record the version under test: `calendar-bridge version`.

---

## 1. First run, from nothing

The path every new user takes. Do it on a clean machine or a clean directory.

- [ ] `go install github.com/dcotelo/calendar-bridge/cmd/calendar-bridge@latest` succeeds, and `calendar-bridge version` reports a version — not `dev`.
- [ ] Following the README quickstart verbatim, without prior knowledge, reaches a first successful sync. Note anywhere you had to guess.
- [ ] `calendar-bridge sync-once` with no config gives a clear error and exit code `3`.
- [ ] `calendar-bridge auth` without `-account` gives a clear error, an example, and exit code `2`.
- [ ] `calendar-bridge auth -account nope` lists the configured account names.
- [ ] A config with one account is rejected with a message explaining that two are needed.

## 2. The authorization flow

- [ ] The printed URL opens, and consent completes.
- [ ] Pasting the **full redirect URL** works.
- [ ] Pasting **just the code** works.
- [ ] Pasting a redirect URL from a *different* `auth` run is refused with a state-mismatch error.
- [ ] Pasting nonsense gives a clear error, not a panic.
- [ ] The token file is created `0600`. Check: `ls -l secrets/`.
- [ ] The token file contains a `refresh_token`.
- [ ] **Re-running `auth` for the same account shows the consent screen again and the new token still has a refresh token.** This is the regression that used to break accounts an hour later.
- [ ] `Ctrl-C` at the paste prompt exits cleanly without writing a partial token file.

## 3. A first real sync

Two accounts, each with a distinctive real event in the next few days.

- [ ] `sync-once -dry-run` reports the blocks it *would* create and creates none. Verify in both calendars.
- [ ] `sync-once` creates them. Verify in both calendars.
- [ ] Each block's title is the configured `block_title`.
- [ ] Each block shows as **Busy**, not Free.
- [ ] Each block is **private**.
- [ ] **The block carries none of the source event's title, description, location, or attendees.** Open it in Google Calendar and confirm.
- [ ] `sync-once` again reports `created: 0, updated: 0, deleted: 0`.

## 4. Event lifecycle

For each, make the change in Google Calendar, run `sync-once`, and check.

- [ ] **Move** an event → its block moves, and is not duplicated.
- [ ] **Shorten** an event → its block shortens.
- [ ] **Delete** an event → its block is removed.
- [ ] **Mark an event Free** → its block is removed, and reported as `skipped`.
- [ ] **Decline an invitation** → its block is removed.
- [ ] **Accept a tentative invitation as "maybe"** → a block still exists.
- [ ] **Create an all-day Busy event** → an all-day block appears.
- [ ] **Create an all-day Free event** (Google's default) → no block.
- [ ] **Create a recurring event** → each instance in the window gets its own block.
- [ ] **Move one instance** of a recurring event → only that instance's block moves.
- [ ] **Delete one instance** of a recurring event → only that instance's block is removed.
- [ ] **Create an event beyond `lookahead_days`** → no block. Then raise `lookahead_days` → the block appears.
- [ ] **Create an event in the past** → no block.
- [ ] **An event in progress right now** → a block exists.

## 5. Three accounts — the loop check

Add a third account.

- [ ] An event on A produces exactly one block on B and one on C.
- [ ] Account A gains **no** blocks of its own.
- [ ] Run five passes. The block count does not grow.
- [ ] Delete the source event → both blocks are removed.

## 6. Failure handling

- [ ] **Revoke** one account at [myaccount.google.com/permissions](https://myaccount.google.com/permissions), then `sync-once`: that account is reported as failing, the others still sync, and **no blocks are deleted anywhere**.
- [ ] Re-authorize it → the next pass recovers with no manual cleanup.
- [ ] **Network down mid-pass** (disable networking during `run`): the pass fails, is logged, and the loop keeps going. Restore networking → the next pass succeeds.
- [ ] **Corrupt a token file** (`echo '{' > secrets/x-token.json`): the error says the token is unreadable, *not* "not yet authorized", exit code `4`.
- [ ] **Delete a token file**: the error says the account needs authorizing, exit code `4`.
- [ ] **`chmod 644` a token file**: startup warns, and still runs.
- [ ] Point `credentials_file` at a nonexistent path → a clear error naming the file.

## 7. The run loop and shutdown

- [ ] `run` performs a pass immediately, not after one `poll_interval`.
- [ ] It keeps polling at the configured interval.
- [ ] **`Ctrl-C` mid-pass**: exits promptly, exit code `0`, no partial-write damage. Run `sync-once` afterwards and confirm it converges.
- [ ] **`SIGTERM`** behaves the same.
- [ ] `kill -9` mid-pass, then `sync-once`: converges with no duplicates and no orphans.
- [ ] `run -dry-run` writes nothing across several passes.

## 8. The web UI

Run `calendar-bridge ui`. Use a real browser.

- [ ] The page loads at `http://127.0.0.1:8090`.
- [ ] **`web_ui.listen_addr: "0.0.0.0:8090"` is refused at startup** with a clear explanation, exit code `3`.
- [ ] Status shows the account count, and says the process is idle rather than implying it is polling.
- [ ] "Sync now" runs a pass, and the status then shows the counts and the timestamp.
- [ ] Editing a field and saving persists to `config.yaml`, which stays `0600`.
- [ ] An invalid poll interval shows the error **on that field** and does not clear what you typed.
- [ ] "Remove" asks for confirmation, and cancelling leaves the row.
- [ ] Removing every account and saving is rejected (fewer than two).
- [ ] With `auth_token` set: the page loads, the API returns 401 until the token is entered, and works afterwards.
- [ ] A wrong token is rejected.
- [ ] **Resize to 380px wide** — everything is usable, nothing overflows horizontally.
- [ ] **Zoom to 200%** — still usable.
- [ ] **Tab through the whole page** — focus is always visible and the order is sensible.
- [ ] Switch the OS between light and dark — the page follows, and text stays readable in both.
- [ ] The browser console is clean.
- [ ] With a screen reader (VoiceOver: ⌘F5), a save and a sync are announced.

## 9. Metrics and health

With `metrics.enabled: true`:

- [ ] `/metrics` parses. Check with `promtool check metrics < <(curl -s localhost:9090/metrics)` if you have it.
- [ ] `/healthz` returns 200 even with every account broken.
- [ ] `/readyz` returns 503 before the first successful pass, 200 after one.
- [ ] `/readyz` returns 503 once `ready_max_age` has elapsed with no success.
- [ ] `calendar_bridge_account_healthy` correctly reports a revoked account as 0.
- [ ] No event titles or account emails appear anywhere in the output.

## 10. Upgrade and rollback

- [ ] Install the previous release, run a pass, then install the new one: it starts against the existing config with no manual migration.
- [ ] `sync-once -dry-run` after upgrading shows the expected changes and nothing surprising.
- [ ] Downgrade to the previous release: it still runs against the same config and converges.

## 11. Deployment targets

Spot-check at least one per release; rotate through them.

- [ ] **Docker**: the documented `docker run` in [deployment/docker.md](deployment/docker.md) works verbatim, copy-pasted, on a clean machine.
- [ ] **docker compose**: `docker compose up -d` works, and `docker compose logs` shows a successful pass.
- [ ] The container runs as non-root: `docker exec <c> id` — or confirm via `docker inspect`.
- [ ] **systemd**: the unit starts, survives a reboot, and `journalctl --user -u calendar-bridge` shows passes.
- [ ] **launchd**: the plist loads, survives a logout/login.
- [ ] **Kubernetes**: `kubectl apply -k deploy/k8s` reaches Ready, and the probes work.
- [ ] **Fly.io**: `fly deploy` reaches a running machine that syncs.
- [ ] **arm64**: the image runs on a Raspberry Pi or Apple silicon.

## 12. Release artifacts

- [ ] Every advertised platform has an archive.
- [ ] `sha256sum -c checksums.txt` passes.
- [ ] `cosign verify-blob` passes for `checksums.txt`.
- [ ] `cosign verify` passes for the container image.
- [ ] The image is multi-arch: `docker manifest inspect ghcr.io/dcotelo/calendar-bridge:<tag>` lists amd64 and arm64.
- [ ] An extracted binary reports the tagged version, not `dev`.
- [ ] The release notes list every behaviour change.

## 13. Privacy — the one that matters most

Do this every release, deliberately.

- [ ] Run a full pass at debug verbosity with real events whose titles you would not want disclosed. Capture all output.
- [ ] **Grep the output for those event titles.** Nothing.
- [ ] Grep for attendee email addresses. Nothing.
- [ ] Grep for the access token and refresh token values. Nothing.
- [ ] Grep for the OAuth client secret. Nothing.
- [ ] Inspect a created block in Google Calendar's UI and via the API: it carries the block title, a time span, and the ownership properties. Nothing else.
- [ ] Capture the process's network traffic for a full pass. Every connection is to a Google endpoint. Nothing else.
