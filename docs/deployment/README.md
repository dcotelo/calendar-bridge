# Deployment

## Choose your deployment

| Target | Effort | Always on? | Cost | Best for |
|---|---|---|---|---|
| [**Laptop**](local.md) (launchd / systemd user unit) | Lowest — 10 min | No, only while awake | Free | Trying it out, and for most people permanently. Start here. |
| [**Docker**](docker.md) | Low — 15 min | While the host is up | Free on hardware you have | A NAS, a home server, or anywhere you already run containers. |
| [**Home server / Raspberry Pi**](home-server.md) | Low — 20 min | Yes | Free after the hardware; ~2 W | You already have a Pi. The nicest always-on option. |
| [**Fly.io**](fly.md) | Medium — 30 min | Yes | ~$2/month | No hardware, no home network exposure, minimal ops. |
| [**Kubernetes**](kubernetes.md) | Highest — 45 min | Yes | Whatever your cluster costs | You already run a cluster. Not worth standing one up for this. |

**If you are not sure, start on your laptop.** calendar-bridge syncs whenever
the machine is awake, which for most people is whenever they are booking
meetings. Move it somewhere always-on later — there is no state to migrate, just
a config file and a token per account.

## What every target needs

1. **A Google Cloud project and OAuth credentials per account.** See the
   [README quickstart](../../README.md#quickstart). About five minutes each.
2. **A token per account, created by `calendar-bridge auth`.** That flow needs a
   browser, so run it on your own machine even when the daemon will live
   elsewhere, then copy the token files to the host. Every guide below covers
   the copying step.
3. **A config file.** See [CONFIGURATION.md](../CONFIGURATION.md).

## The mistake almost everyone makes

**Paths in `config.yaml` are resolved against the process's working directory,
not against the config file.**

On a laptop, running from the directory that holds `config.yaml`, relative paths
work and nobody notices. In a container the working directory is `/app`, so
`secrets/personal-token.json` means `/app/secrets/personal-token.json` — not
`/app/config/secrets/personal-token.json`, where you probably mounted it.

**Use absolute paths anywhere the working directory is not obvious.** Every
guide below does.

## Verify it is working, anywhere

```bash
# Validate the config, authenticate every account, fetch events, and report what
# a real pass would change — without writing to any calendar.
calendar-bridge sync-once -config /path/to/config.yaml -dry-run

# Machine-readable.
calendar-bridge sync-once -config /path/to/config.yaml -dry-run -json
```

Then create a test event on one calendar, wait one `poll_interval`, and look for
a `Busy (calendar-bridge)` block on the others.

## Monitoring

Every guide shows how to enable `/metrics`, `/healthz` and `/readyz`. The one
alert worth having is "no successful sync in three poll intervals" — see
[OBSERVABILITY.md](../OBSERVABILITY.md#alerting).

## Removing it cleanly

Every guide has an uninstall section. All of them end with the same manual step,
because it cannot be automated yet:

**Busy blocks are not removed when you stop running calendar-bridge.** Garbage
collection only removes blocks it can see and match to a missing source; a
process that is not running collects nothing. Search each calendar for your
`block_title` and delete the results. A clean-uninstall command is on the
roadmap.
