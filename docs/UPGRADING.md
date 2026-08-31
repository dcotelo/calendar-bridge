# Upgrading

## Versioning policy

calendar-bridge is **pre-1.0**. Versions are `v0.MINOR.PATCH`:

- **PATCH** — bug fixes and internal changes. No config changes, no behaviour
  changes you would notice.
- **MINOR** — new features, and occasionally a change in what gets synced. Any
  behaviour change that affects which blocks exist is called out in the release
  notes and listed below.
- **MAJOR** stays `0` until the config schema and the sync semantics are stable.

Breaking changes are prefixed `feat!:` or `fix!:` and appear at the top of the
release notes.

## Before you upgrade

Check what you have:

```bash
calendar-bridge version
```

Read the release notes between your version and the target — behaviour changes
that alter which blocks exist are always listed.

## The general procedure

Every deployment guide has a target-specific version. The shape is always:

1. Note the current version, so you can roll back to it.
2. Install the new binary or pull the new image.
3. Run a dry run — it writes nothing and tells you what would change:

   ```bash
   calendar-bridge sync-once -config config.yaml -dry-run -json | python3 -m json.tool
   ```

4. If the counts look wrong, roll back before restarting the daemon.
5. Restart.
6. Verify:

   ```bash
   curl -s http://127.0.0.1:9090/readyz
   ```

The dry run is the important step. It is the difference between finding a
surprise on a laptop and finding it on your calendar.

## Rolling back

There is no state to migrate: calendar-bridge keeps everything in the calendars
themselves, as ownership tags. Rolling back is just running the older binary
again.

The exception is if the newer version *changed which blocks exist*. Rolling back
does not undo that — the older version will see the new state and reconcile it
to what it thinks is right, producing another pass of writes. That is safe but
noisy.

```bash
# binaries
curl -fsSL -o calendar-bridge.tar.gz \
  https://github.com/dcotelo/calendar-bridge/releases/download/v0.1.0/calendar-bridge_linux_amd64.tar.gz
tar xzf calendar-bridge.tar.gz

# containers
docker pull ghcr.io/dcotelo/calendar-bridge:0.1.0
```

Always roll back to an exact version, never to a floating tag.

## Verifying a download

Releases carry keyless cosign signatures and SBOMs.

```bash
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/dcotelo/calendar-bridge/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# Linux:
sha256sum -c checksums.txt --ignore-missing
# macOS (no GNU coreutils; shasum ships with the system):
shasum -a 256 -c checksums.txt --ignore-missing
```

For the container image:

```bash
cosign verify ghcr.io/dcotelo/calendar-bridge:0.1.0 \
  --certificate-identity-regexp '^https://github\.com/dcotelo/calendar-bridge/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

---

## Version-specific notes

### Upgrading to the current development version

Several changes alter behaviour in ways you will see. None require a config
change — every new key has a default that preserves the previous behaviour
except where noted.

**Events marked Free and declined invitations no longer create blocks.**
Previously every non-cancelled event produced a block. On your first pass after
upgrading, expect blocks to be **removed** for any source event you had marked
"Free" or declined. This is the intended fix, but it is a visible one-time
change. Preview it first:

```bash
calendar-bridge sync-once -config config.yaml -dry-run -json | python3 -m json.tool
```

A large `deleted` count on that first dry run is expected if you decline a lot
of meetings. Tentative invitations still block time.

**`block_title` changes now propagate to existing blocks.** Previously only new
blocks got a changed title. If you ever changed `block_title`, the first pass
after upgrading updates every block that still carries the old one — one noisy
pass, then quiet.

**Re-running `auth` now forces the consent prompt.** This fixes accounts silently
losing their refresh token on re-authorization. You will see Google's consent
screen every time you run `auth`, including for an account you already
authorized. That is deliberate.

**Refreshed tokens are now written back to disk.** The token file is rewritten
whenever Google issues a new access or refresh token. Make sure the directory is
writable by the process — a read-only secrets mount now logs a warning on every
refresh. See the Kubernetes and Docker guides for the recommended layout.

**New optional config blocks.** `metrics` (Prometheus endpoint and health
probes) is off by default. Nothing changes unless you enable it.

**New CLI surface.** `version`, `-dry-run`, `-json`, and documented exit codes
(`2` usage, `3` config, `4` auth, `5` sync failure, `6` runtime) replacing a
blanket `1`. If you have scripts that branch on a non-zero exit, they still
work; if they branch on exactly `1`, update them.

### From a version before signed releases

Older releases have no cosign signatures. Verify checksums manually, and treat
the first signed release as the point from which verification is possible.
