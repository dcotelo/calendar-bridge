# Troubleshooting

One section per symptom. Each has the diagnosis commands to run and the fix.

The single most useful command when something is wrong:

```bash
calendar-bridge sync-once -config config.yaml -dry-run
```

It validates the config, authenticates every account, fetches events, and
reports exactly what a real pass would change — **without writing to any
calendar**. Safe to run at any time, including while the daemon is running.

---

## `account not yet authorized`

```text
setting up: account personal: account not yet authorized, run: calendar-bridge auth -account <account-name>
```

Exit code `4`.

**Cause.** The token file for that account does not exist. Either the account
was never authorized, or the file was moved, deleted, or is being looked for at
the wrong path.

**The error names the account, not the path — deliberately.** These messages
reach the systemd journal and `docker logs`, so they never disclose where on
disk your credentials live. That means you cannot read the resolved path out of
the error; you have to look at the config yourself. The commands below do that.

**Diagnose.**

```bash
# 1. Read the token_file for that account out of the config. This parses the
#    YAML rather than grepping near a line: key order is not guaranteed and a
#    comment between the keys would break a line-offset match.
TOKEN=$(python3 -c '
import sys, yaml
cfg = yaml.safe_load(open("config.yaml"))
name = sys.argv[1]
for a in cfg["accounts"]:
    if a["name"] == name:
        print(a["token_file"]); break
else:
    sys.exit("no account named " + name)
' personal)
echo "$TOKEN"

# 2. Does the file exist there?
ls -l "$TOKEN"

# 3. If that path is RELATIVE, it resolves against the working directory of the
#    process — not against config.yaml. This is the most common cause under
#    systemd and Docker, where the working directory is not where you think.
pwd
```

If you do not have PyYAML, just open `config.yaml` and read the `token_file`
value for that account — that is all the snippet does.

**Fix.** Run the authorization flow for that account:

```bash
calendar-bridge auth -config config.yaml -account personal
```

If the file *does* exist, the path is being resolved differently than you
expect. This is the classic container failure — see
[the Docker guide](deployment/docker.md#the-most-common-first-run-failure).
Use absolute paths.

---

## `token file exists but could not be read as an OAuth2 token`

Exit code `4`.

**Cause.** The token file is present but corrupt: truncated by an interrupted
write from an older version, hand-edited, or overwritten with something else.
This is deliberately reported differently from "not yet authorized" because the
fix differs.

**Diagnose.** As above, the error names the account rather than the path, so
read the path out of the config:

```bash
# Resolved the same YAML-aware way as the section above.
TOKEN=$(python3 -c '
import sys, yaml
cfg = yaml.safe_load(open("config.yaml"))
for a in cfg["accounts"]:
    if a["name"] == sys.argv[1]:
        print(a["token_file"]); break
else:
    sys.exit("no account named " + sys.argv[1])
' personal)
# A valid token file is one JSON object with access_token and refresh_token.
python3 -m json.tool < "$TOKEN"
```

**Fix.** Delete it and re-authorize:

```bash
# $TOKEN as resolved above — a hard-coded path repairs the wrong file for
# anyone whose config points somewhere else.
rm "$TOKEN"
calendar-bridge auth -config config.yaml -account personal
```

Current versions write token files atomically, so an interrupted write can no
longer produce this.

---

## The account stops working about an hour after `auth`

**Cause.** The saved token has no refresh token, so it dies when the access
token expires.

Google issues a refresh token for an installed-app flow **only on the first
authorization** of a given client and user, unless the consent prompt is forced.
Older versions of calendar-bridge did not force it, so re-running `auth` on an
already-granted account produced an access-token-only file — a time bomb.

**Diagnose.**

```bash
# "$TOKEN" as resolved in the section above, not a fixed path.
python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print('refresh_token present:', bool(d.get('refresh_token')))" "$TOKEN"
```

Startup also warns:

```text
level=WARN msg="token file has no refresh token; this account will stop working when its access token expires"
```

**Fix.** Current versions force the consent prompt and refuse to save a token
with no refresh token, so simply re-running `auth` is now enough:

```bash
calendar-bridge auth -config config.yaml -account personal
```

If it still comes back without one, revoke the grant at
[myaccount.google.com/permissions](https://myaccount.google.com/permissions) and
authorize again from scratch.

---

## `invalid_grant` / the account worked for days and then stopped

**Cause, most likely.** Your OAuth consent screen is in **Testing** publishing
status, where Google expires refresh tokens after **seven days**. This catches
almost everyone who follows the setup guide without changing the publishing
status.

Other causes: you revoked the grant, changed your Google password with
"sign out of all sessions", or the account is subject to a Workspace admin
policy that revoked third-party access.

**Diagnose.**

Open Google Cloud Console → *APIs & Services → OAuth consent screen* and look
at **Publishing status**.

```bash
calendar-bridge sync-once -config config.yaml -dry-run
```

**Fix.** Set the publishing status to **In production**. For a personal app used
only by you, this needs no verification review — you will see an "unverified
app" interstitial during `auth`, which you click through with *Advanced →
Go to (app)*. Then re-authorize:

```bash
calendar-bridge auth -config config.yaml -account personal
```

Staying in Testing means re-authorizing every account every week, forever.

---

## `fewer than 2 healthy accounts`

Exit code `5`.

**Cause.** Two or more accounts failed to fetch this pass, leaving fewer than
two to sync between. calendar-bridge aborts rather than acting on partial data.

**Diagnose.** The joined error names every failing account. With metrics on:

```bash
curl -s http://127.0.0.1:9090/metrics | grep account_healthy
```

**Fix.** Depends on the per-account error — usually expired tokens (above) or a
network problem. Note this is a *symptom*: fix the underlying account failures.

---

## Busy blocks are not appearing

Work through these in order.

**1. Has a pass actually run?**

```bash
curl -s http://127.0.0.1:9090/readyz
journalctl --user -u calendar-bridge -n 50 | grep sync.pass
```

**2. Is the source event inside the sync window?** The window is
`[now − 24h, now + lookahead_days]`. An event further out than
`lookahead_days` has no block yet, by design.

**3. Is the event marked "Free"?** calendar-bridge deliberately does not create
a block for an event whose transparency is "Free" — you said it does not consume
your time.

**4. Did you decline the invitation?** Declined invitations also produce no
block, deliberately.

Both show in the pass summary as `skipped`:

```bash
calendar-bridge sync-once -config config.yaml -dry-run -json | python3 -m json.tool
```

**5. Are you looking at the right calendar?** Blocks are written to the
`calendar_id` in the config — `primary` by default. If you configured a
secondary calendar, the blocks are there.

**6. Is the block created but invisible?** Blocks are created with
`visibility: private`. You see them on your own calendar; someone with only
free/busy access to you sees the time as busy without the title.

---

## Duplicate busy blocks

**One block plus one real event on the same slot.** Expected, not a bug: if you
are invited to the same meeting on two configured accounts, each account's copy
is a real event that produces a block on the other. calendar-bridge does not yet
deduplicate across accounts by `iCalUID`; it is on the roadmap.

**Two identical blocks for one source event.** This should not happen —
insertion is idempotent and reconciles ambiguous failures. If you see it:

```bash
calendar-bridge sync-once -config config.yaml -dry-run -json | python3 -m json.tool
```

A steady state should report `created: 0, updated: 0, deleted: 0`. If it keeps
creating, please [open an issue](https://github.com/dcotelo/calendar-bridge/issues)
with that output. A likely cause is a duplicated block created by hand in Google
Calendar's UI: "Duplicate event" copies the private extended properties, so two
events end up claiming the same source. Delete the copy.

---

## Blocks keep being rewritten every pass

A settled pass should perform zero writes.

**Diagnose.**

```bash
calendar-bridge sync-once -config config.yaml -json | python3 -m json.tool
# then immediately again
calendar-bridge sync-once -config config.yaml -json | python3 -m json.tool
```

The second should report all zeros.

**Cause.** If `updated` is non-zero every time, something is making the block
look different from what the engine expects each pass. Changing `block_title`
causes exactly one pass of updates — that is correct. If it repeats forever,
please open an issue with both outputs.

---

## Blocks were left behind after I stopped using it

**Cause.** Garbage collection only removes blocks it can currently see and match
to a *missing* source. If you stop running calendar-bridge, delete `config.yaml`,
or remove an account from the config, the blocks it already created stay where
they are. There is no clean-uninstall command yet — it is on the roadmap.

**Fix.** delete only events carrying the private extended property
`calendarBridgeOwner=calendar-bridge`, which no human event has. **Do not delete
by `block_title`** — it is configurable, it is ordinary text on the event, and a
title search will also match real events someone created with the same name. See
[Removing it cleanly](deployment/README.md#removing-it-cleanly) for the API query. In
Google Calendar's search box:

```text
Busy (calendar-bridge)
```

The uninstall section of each deployment guide walks through this.

---

## Push notifications are not arriving

Only relevant with `webhook.enabled: true`.

**Diagnose.**

```bash
# Is the public URL reachable from outside, over HTTPS?
curl -i https://cb.example.com/webhook

# Did the watch channels register?
journalctl --user -u calendar-bridge | grep 'watch channel'
```

A `POST` with no token should get `403`. A `GET` should get `405`. Anything else
— a 502, a certificate error, a redirect — means Google cannot deliver either.

**Common causes.** The endpoint is not reachable from the public internet;
the certificate is self-signed or expired (Google requires a valid one); the
reverse proxy is not forwarding `/webhook` to `listen_addr`; the domain does not
resolve publicly.

Polling continues regardless, so this degrades latency, never correctness.

---

## The web UI will not start

```text
ui: refusing to bind non-loopback address "0.0.0.0:8090"
```

Exit code `3`. **Not a bug.** The UI serves plaintext HTTP and can rewrite your
config, so a non-loopback bind would put the auth token and the config on the
wire in the clear. It is refused unconditionally.

**Fix.** Bind loopback and tunnel:

```bash
# on the server
calendar-bridge ui -config config.yaml   # 127.0.0.1:8090

# on your laptop
ssh -L 8090:127.0.0.1:8090 you@your-server
# then open http://127.0.0.1:8090
```

```text
ui: web_ui.enabled is false in config; set it to true to run the UI
```

Add `web_ui: {enabled: true}` to the config.

---

## `unexpected Host` from the web UI

Exit is not affected; the API returns `403`.

**Cause.** The DNS-rebinding guard. In the default no-token mode, requests whose
`Host` header is not a loopback authority are rejected — otherwise a page on a
hostname that resolves to `127.0.0.1` could drive the API from your browser.

**Fix.** Reach the UI as `http://127.0.0.1:8090` or `http://localhost:8090`. If
you are going through a reverse proxy that rewrites `Host`, set
`web_ui.auth_token`: with a token configured, the token is the guard and the
Host constraint is not applied.

---

## High Google API usage

**Diagnose.** Google Cloud Console → *APIs & Services → Google Calendar API →
Quotas*.

**Causes and fixes.**

- `poll_interval` too short. Each pass costs one `events.list` per account. `30s`
  across five accounts is 14,400 calls a day before any writes.
- Many accounts. Propagation is between every pair, so work grows with the
  square of the account count.
- Something is rewriting blocks every pass. See above — a settled pass should
  perform zero writes.

Enable [webhooks](webhooks.md) to get low latency with a long `poll_interval`,
rather than buying latency with a short one.

---

## Permission warnings at startup

```text
level=WARN msg="secret file has insecure permissions; restrict it to owner-only (chmod 600)" kind=token file=personal-token.json mode=-rw-r--r--
```

**Fix.**

```bash
chmod 600 secrets/*.json
chmod 700 secrets
```

calendar-bridge warns rather than refusing to start, so this cannot become a
crash loop on a host where you loosened permissions deliberately.

---

## Still stuck

Gather this before opening an issue:

```bash
calendar-bridge version
calendar-bridge sync-once -config config.yaml -dry-run -json 2>&1 | tail -40
```

Then [open an issue](https://github.com/dcotelo/calendar-bridge/issues/new/choose).
**Redact your account names if they identify an employer, and never paste the
contents of a token or credentials file.** For anything that looks like a
security vulnerability, follow [SECURITY.md](../SECURITY.md) instead.
