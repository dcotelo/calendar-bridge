# Deploying on your laptop

The lowest-effort option, and a perfectly good permanent one. calendar-bridge
syncs whenever the machine is awake — which, for most people, is whenever they
are booking meetings.

**Trade-off:** nothing syncs while the laptop is asleep or off. A meeting booked
on your work calendar overnight will not block your personal calendar until you
open the lid. If that matters, use an [always-on target](README.md).

---

## Prerequisites

- Go 1.25+ (to build), or a [released binary](https://github.com/dcotelo/calendar-bridge/releases).
- OAuth credentials for each Google account. See the
  [quickstart](../../README.md#quickstart).

---

## 1. Install

```bash
go install github.com/dcotelo/calendar-bridge/cmd/calendar-bridge@latest
calendar-bridge version
```

That puts the binary in `$(go env GOPATH)/bin`. Or download a release:

```bash
# macOS, Apple silicon
curl -fsSL -o cb.tar.gz https://github.com/dcotelo/calendar-bridge/releases/latest/download/calendar-bridge_darwin_arm64.tar.gz
tar xzf cb.tar.gz
sudo install -m 755 calendar-bridge /usr/local/bin/
```

Verify the download first — see [UPGRADING.md](../UPGRADING.md#verifying-a-download).

## 2. Set up the config directory

```bash
mkdir -p ~/.config/calendar-bridge/secrets
chmod 700 ~/.config/calendar-bridge/secrets
cd ~/.config/calendar-bridge
```

Put each downloaded credentials JSON in `secrets/`, then write `config.yaml`
with **absolute paths** — a launchd or systemd unit does not run from this
directory:

```yaml
accounts:
  - name: personal
    credentials_file: /Users/you/.config/calendar-bridge/secrets/personal-credentials.json
    token_file: /Users/you/.config/calendar-bridge/secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /Users/you/.config/calendar-bridge/secrets/work-acme-credentials.json
    token_file: /Users/you/.config/calendar-bridge/secrets/work-acme-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"

metrics:
  enabled: true
  listen_addr: "127.0.0.1:9090"
```

On Linux, substitute `/home/you`.

```bash
chmod 600 config.yaml
```

## 3. Authorize each account

```bash
cd ~/.config/calendar-bridge
calendar-bridge auth -config config.yaml -account personal
calendar-bridge auth -config config.yaml -account work-acme
```

Open the printed URL, approve, then paste back either the code or the full
redirect URL your browser lands on.

## 4. Check it before automating it

```bash
calendar-bridge sync-once -config config.yaml -dry-run
```

**You should now see** a line per account reporting how many real events and
owned blocks it found, then a summary of what would be created. Nothing has been
written yet.

Then do it for real:

```bash
calendar-bridge sync-once -config config.yaml
```

**You should now see** `Busy (calendar-bridge)` blocks on each calendar,
mirroring the other's events. Check in Google Calendar before continuing.

---

## 5a. macOS — launchd

`deploy/launchd/dev.dcotelo.calendar-bridge.plist` is in the repo. Install it:

```bash
mkdir -p ~/Library/LaunchAgents ~/Library/Logs
sed -e "s|__HOME__|$HOME|g" \
    -e "s|__BINARY__|$(command -v calendar-bridge)|g" \
    deploy/launchd/dev.dcotelo.calendar-bridge.plist \
  > ~/Library/LaunchAgents/dev.dcotelo.calendar-bridge.plist

launchctl load -w ~/Library/LaunchAgents/dev.dcotelo.calendar-bridge.plist
```

Verify:

```bash
launchctl list | grep calendar-bridge     # a PID and exit code 0
tail -f ~/Library/Logs/calendar-bridge.log
curl -s http://127.0.0.1:9090/readyz
```

Manage it:

```bash
launchctl kickstart -k gui/$(id -u)/dev.dcotelo.calendar-bridge   # restart
launchctl unload ~/Library/LaunchAgents/dev.dcotelo.calendar-bridge.plist  # stop
```

**macOS specifics.** A `LaunchAgent` runs only while you are logged in — that is
what you want here, since it needs your home directory. It does not run while
the Mac is asleep; `KeepAlive` restarts it on wake. If you want it to survive
sleep more aggressively, `caffeinate` is not the answer — use an always-on host.

## 5b. Linux — systemd user unit

A **user** unit, not a system one: the tokens live in your home directory and
the process needs no privileges.

```bash
mkdir -p ~/.config/systemd/user
sed "s|__BINARY__|$(command -v calendar-bridge)|g" \
  deploy/systemd/calendar-bridge.service > ~/.config/systemd/user/calendar-bridge.service

systemctl --user daemon-reload
systemctl --user enable --now calendar-bridge

# Keep it running when you are not logged in.
sudo loginctl enable-linger "$USER"
```

Verify:

```bash
systemctl --user status calendar-bridge
journalctl --user -u calendar-bridge -f
curl -s http://127.0.0.1:9090/readyz
```

The unit sets `NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`,
`ProtectHome=read-only` with a read-write exception for the config directory
(the token files must be rewritable, since tokens are re-persisted on refresh),
and a restricted syscall set.

---

## Upgrading

```bash
# 1. Note what you are on, so you can go back.
calendar-bridge version

# 2. Install the new one.
go install github.com/dcotelo/calendar-bridge/cmd/calendar-bridge@latest

# 3. Preview. This writes nothing.
calendar-bridge sync-once -config ~/.config/calendar-bridge/config.yaml -dry-run

# 4. Restart.
launchctl kickstart -k gui/$(id -u)/dev.dcotelo.calendar-bridge   # macOS
systemctl --user restart calendar-bridge                          # Linux
```

Read [UPGRADING.md](../UPGRADING.md) first — some versions change which blocks
exist, and the dry run in step 3 is where you find that out safely.

## Rolling back

```bash
go install github.com/dcotelo/calendar-bridge/cmd/calendar-bridge@v0.1.0
# then restart as above
```

There is no state to migrate. If the newer version changed which blocks exist,
rolling back will produce one reconciling pass.

## Uninstalling cleanly

```bash
# 1. Stop it.
launchctl unload ~/Library/LaunchAgents/dev.dcotelo.calendar-bridge.plist   # macOS
rm ~/Library/LaunchAgents/dev.dcotelo.calendar-bridge.plist
systemctl --user disable --now calendar-bridge                              # Linux
rm ~/.config/systemd/user/calendar-bridge.service && systemctl --user daemon-reload

# 2. Revoke access, so the tokens are dead even if a copy survives.
#    https://myaccount.google.com/permissions

# 3. Remove the files.
rm -rf ~/.config/calendar-bridge
rm -f "$(command -v calendar-bridge)"
```

**4. Remove the busy blocks it created.** This is manual — a stopped process
collects nothing. In Google Calendar, search for your `block_title`
(`Busy (calendar-bridge)`) on each account and delete the results.

---

## Failure modes specific to this target

| Symptom | Cause |
|---|---|
| Nothing synced overnight | The machine was asleep. Expected. |
| launchd shows a non-zero exit | `~/Library/Logs/calendar-bridge.err.log` has the reason. Usually a wrong absolute path in the plist or the config. |
| systemd unit fails with a permission error | `ProtectHome=read-only` with the wrong `ReadWritePaths`. The token directory must be writable — tokens are re-persisted on refresh. |
| Works interactively, fails under launchd/systemd | Almost always relative paths in `config.yaml`. The service does not run from your shell's directory. Use absolute paths. |
| Stops after a week with `invalid_grant` | The OAuth consent screen is in **Testing**, where refresh tokens expire after 7 days. Set it to **In production**. See [TROUBLESHOOTING.md](../TROUBLESHOOTING.md#invalid_grant--the-account-worked-for-days-and-then-stopped). |
