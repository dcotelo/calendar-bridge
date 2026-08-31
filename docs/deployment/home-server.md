# Deploying on a home server or Raspberry Pi

The nicest always-on option if you already own the hardware: no monthly cost,
nothing leaves your house, and a Pi draws about 2 W.

Works on a Pi 3 or later, any x86 mini-PC, or a NAS that runs containers.

---

## Resource footprint

Measured on a Raspberry Pi 4 with three accounts and a 30-day window:

| | |
|---|---|
| Memory | ~25 MB resident, idle and during a pass |
| CPU | Effectively zero between passes; well under a second of CPU per pass |
| Disk | ~15 MB binary, a few KB of config and tokens |
| Network | A few hundred KB per pass |

A Pi Zero 2 W is enough. This is not a demanding workload.

---

## Prerequisites

- 64-bit OS. Check with `uname -m`: you want `aarch64`. A 32-bit Raspberry Pi OS
  reports `armv7l` — released binaries and images are **arm64 only**, so
  reinstall with the 64-bit image, or build from source.
- OAuth credentials per account, and **tokens created on a machine with a
  browser** — the `auth` flow is interactive.

---

## Option A — a binary and a systemd user unit

Lightest, and the easiest to debug.

```bash
uname -m     # expect aarch64

curl -fsSL -o cb.tar.gz \
  https://github.com/dcotelo/calendar-bridge/releases/latest/download/calendar-bridge_linux_arm64.tar.gz
tar xzf cb.tar.gz
sudo install -m 755 calendar-bridge /usr/local/bin/
calendar-bridge version
```

Then follow [the Linux section of the local guide](local.md#5b-linux--systemd-user-unit)
— a systemd **user** unit with `loginctl enable-linger`, so it runs whether or
not you are logged in.

Copy your token files across:

```bash
mkdir -p ~/.config/calendar-bridge/secrets
chmod 700 ~/.config/calendar-bridge/secrets
scp you@laptop:~/.config/calendar-bridge/secrets/*.json ~/.config/calendar-bridge/secrets/
chmod 600 ~/.config/calendar-bridge/secrets/*.json
```

Use **absolute paths** in `config.yaml` — a systemd unit does not run from your
shell's directory.

## Option B — Docker

If the box already runs containers, follow [the Docker guide](docker.md). The
image is multi-arch, so `ghcr.io/dcotelo/calendar-bridge:latest` pulls arm64
automatically.

Consider lowering the memory limit:

```yaml
deploy:
  resources:
    limits:
      memory: 96M
```

---

## Low-resource notes

- **Do not shorten `poll_interval` to compensate for a slow machine.** The pass
  is network-bound, not CPU-bound; a shorter interval just makes more calls.
- **SD cards wear out.** calendar-bridge writes very little — a token file
  roughly hourly per account — but if you are already worried about your card,
  put `~/.config/calendar-bridge` on a USB SSD.
- **Log rotation.** With systemd this is handled by the journal. Cap it if you
  are tight on space: `journalctl --vacuum-size=100M`.
- **Time.** A Pi with no RTC gets its clock from NTP at boot. If the clock is
  badly wrong, Google rejects the OAuth token as expired or not-yet-valid.
  `timedatectl status` should show the clock synchronised.
- **Power loss.** Nothing to corrupt: config and token writes are atomic, and a
  pass interrupted halfway is safe because every operation is idempotent.

## Monitoring

```yaml
metrics:
  enabled: true
  # Reachable from your LAN, not from the internet.
  listen_addr: "0.0.0.0:9090"
```

**Do not port-forward this from your router.** It is read-only and
unauthenticated. Keeping it on the LAN is the point of running it at home.

Check it from another machine on the network:

```bash
curl -s http://raspberrypi.local:9090/readyz
```

If you do not run Prometheus, a cron job is enough:

```bash
# every 30 minutes
*/30 * * * * curl -fsS http://127.0.0.1:9090/readyz >/dev/null || echo "calendar-bridge is not syncing" | mail -s "calendar-bridge" you@example.com
```

---

## Upgrading, rolling back, uninstalling

Identical to [the local guide](local.md#upgrading) for Option A, or
[the Docker guide](docker.md#upgrading) for Option B.

Do not forget the last step in either case: **the busy blocks it created are not
removed when you stop it.** delete only events carrying the private extended property
`calendarBridgeOwner=calendar-bridge`, which no human event has. **Do not delete
by `block_title`** — it is configurable, it is ordinary text on the event, and a
title search will also match real events someone created with the same name. See
[Removing it cleanly](README.md#removing-it-cleanly) for the API query.

---

## Failure modes specific to this target

| Symptom | Cause and fix |
|---|---|
| `exec format error` | A 32-bit OS. `uname -m` must report `aarch64`. Reinstall 64-bit, or `go build` on the Pi. |
| `oauth2: token expired` right after a reboot | The clock was wrong before NTP synchronised. `timedatectl status`. Consider an RTC module, or `systemd-time-wait-sync.service`. |
| Stops after a reboot | You did not `loginctl enable-linger`, so the user unit does not start until you log in. |
| Very slow passes | Usually DNS or a saturated uplink, not the CPU. `time calendar-bridge sync-once -config config.yaml -dry-run`. |
| Filesystem went read-only | SD card failure. Replace it, and consider a USB SSD. |
| Works on the LAN but not after a router change | Nothing about calendar-bridge needs inbound connectivity — only outbound HTTPS. Check egress, not ingress. |
