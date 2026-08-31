# FAQ

## What does it actually do?

You have a personal Google account and one or more work Workspace accounts.
Someone books you for a meeting at 2pm on your work calendar. Your personal
calendar has no idea, so someone else books you at 2pm there too.

calendar-bridge watches all of them and, for every real event on one calendar,
puts a generic "Busy" placeholder at the same time on every other calendar. Now
both sides see you are unavailable.

## Why not use OneCal, Reclaim, SyncThemCalendars, or similar?

They work well and are less effort than running your own. The trade is that your
event data flows through their servers.

For a lot of people that is fine. If you would rather it did not — because of a
work policy, a client NDA, or plain preference — calendar-bridge does the same
job entirely on infrastructure you control. Nothing leaves your machine except
ordinary Calendar API calls to Google, which already has your calendar.

That is the whole argument. If it does not move you, use a hosted tool.

## Does Google not already do this?

Within one account, yes: the "Other calendars" sidebar overlays calendars you
have access to. Across separate accounts — and especially across different
Workspace domains with different admins — there is no native way to make one
account's busy time block another's.

## What information crosses between my accounts?

A start time, an end time, the fixed `block_title` string, and the bookkeeping
that lets a block be matched back to its source.

**No event content.** Titles, descriptions, locations, attendees and
conferencing links are never copied. That is structural, not a setting: the
internal event model has no fields for them.

The bookkeeping is three private properties written onto the block: which
account it came from (the name you chose in `config.yaml`), that account's
`calendar_id`, and Google's opaque ID for the source event. They are what makes
garbage collection safe — a block can be proven to belong to a source that is
gone — so they are not optional. Full detail in
[what crosses an account boundary](ARCHITECTURE.md#what-crosses-an-account-boundary).

A colleague who can see your work calendar learns *when* you are busy — which is
the point, and the same thing a Google free/busy share tells them. They do not
learn what you are doing. If they can read the event's properties they can also
see which of your accounts the block came from, though not what the event was.

## Will it delete my real events?

No, and this is the invariant the whole design exists to protect.

Every block calendar-bridge creates is tagged with a private
`calendarBridgeOwner` extended property. Garbage collection only ever considers
tagged events. Deletion is additionally conditional on the ETag read moments
earlier, so an event modified in between fails its precondition instead of being
removed. The ownership check is re-verified at every layer rather than trusted
once, and the test suite attacks it directly — including with events that
deliberately carry matching source properties but no owner tag.

## Can it get into a sync loop?

No. A block is never classified as a real event, so it is never propagated
onward. With three accounts, an event on A produces exactly one block on B and
one on C — never a block-of-a-block. There is a test that runs repeated passes
across three accounts and fails if the block count ever grows.

## How fast does a change propagate?

By default, up to `poll_interval` (5 minutes). Enable
[webhooks](webhooks.md) for near-instant propagation; polling stays on as a
safety net.

## Does it work with Outlook, iCloud, or CalDAV?

Not yet. The internal `Provider` interface exists specifically so a non-Google
backend can be added without touching the engine, and the Google adapter is the
template — but no other provider is implemented. Tracked on the roadmap.

## What if I decline a meeting?

No block is created, and an existing block for it is removed on the next pass.
Declining a meeting and still losing the slot everywhere else would be the most
annoying thing this tool could do.

## What about events I mark "Free"?

Same: no block. You explicitly said it does not consume your time.

## What about "maybe" / tentative invitations?

Tentative invitations **do** block time. That is a deliberate choice: a maybe is
a real risk of being busy, and holding a slot you turn out not to need is much
milder than double-booking one you do.

## What about all-day events?

An all-day event that is marked Busy produces an all-day block on the other
calendars. Most all-day events in Google Calendar default to "Free", so a
birthday or a public holiday produces nothing.

If you mark a multi-day holiday as Busy and do not want your whole working week
blanked elsewhere, mark it Free instead. Making this a config option is on the
roadmap.

## What about recurring events?

Handled. calendar-bridge asks Google to expand recurring events into individual
instances, so each occurrence in the window is a separate event with its own
block. A single moved or cancelled instance is treated correctly, because Google
reports it as its own instance.

## How many accounts can I use?

Two minimum. Three to five is comfortable. There is no hard limit, but the cost
model matters: each pass makes one API call per account, and propagation is
between every pair, so work grows with the square of the account count.

## Do I need a server?

No. It runs perfectly well on a laptop under `launchd` or a systemd user unit —
it only syncs while the machine is awake, which is usually fine. An always-on
host gets you propagation while you sleep. See
[the deployment guide](deployment/README.md).

## How much does it cost to run?

The software is MIT-licensed and free. The Google Calendar API is free at this
volume. Hosting is whatever you choose — nothing at all on a machine you
already have, or roughly $2/month on the smallest Fly.io machine.

## Is a Google Cloud project really necessary?

Yes. OAuth needs a client ID, and Google issues those per project. It is free
and takes about five minutes per account.

## Why do I have to click through an "unverified app" warning?

Because you are the developer of an app used only by you, and Google's
verification review is for apps with real users. Clicking *Advanced → Go to
(app)* is the normal path.

Do set your consent screen's publishing status to **In production** rather than
leaving it in **Testing** — in Testing, Google expires refresh tokens after
seven days and you will be re-authorizing every account every week.

## Where are my tokens stored, and how safely?

In the `token_file` paths from your config, as plaintext JSON at `0600`, written
atomically. Anyone who can read those files has your calendars until you revoke
the grant.

Encrypted-at-rest storage (OS keychain, age, systemd credentials) is a known gap
on the roadmap. See [THREAT-MODEL.md](THREAT-MODEL.md).

## What happens if my token expires or I revoke access?

That account is excluded from the pass entirely — no blocks are pushed to it,
and no blocks mirroring its events are collected elsewhere, because
calendar-bridge cannot tell "this event was deleted" from "I failed to look".
Every other account keeps syncing. The failure is reported in the logs, in the
web UI, and in `calendar_bridge_account_healthy`.

## Can I run it on more than one machine at once?

You can, and it will not corrupt anything: every operation is idempotent and
insertion is deduplicated by source identity. But you will double your API
usage for no benefit. Don't.

## How do I remove every block it ever created?

Using the Calendar API, list events with the private extended property
`calendarBridgeOwner=calendar-bridge` and delete those.

**Do not search by `block_title`.** It is configurable and is ordinary text on
the event, so a title search also matches real events someone created with the
same name — and deleting those is unrecoverable. The ownership property is the
only safe discriminator, and it is the same one the engine checks before it
modifies anything.

There is no `uninstall` command yet; it is backlog item 1. Each deployment
guide has an uninstall section, and
[Removing it cleanly](deployment/README.md#removing-it-cleanly) has the exact
API query.

## Does it phone home?

No. No telemetry, no analytics, no update check, no crash reporting. The only
outbound connections are to `accounts.google.com` and `*.googleapis.com`.

## Is the web UI required?

No. It is off by default and purely a convenience for editing `config.yaml`
without hand-editing YAML. Everything it does can be done with a text editor and
the CLI.

## Can I expose the web UI on my network?

No — a non-loopback bind is refused unconditionally. It serves plaintext HTTP,
so exposing it would put the auth token and your config on the wire in the
clear. Use an SSH tunnel or a TLS-terminating reverse proxy pointed at the
loopback port.

## Why Go?

A single static binary with no runtime, which is the right shape for something
you install on a Raspberry Pi, a Fly machine, and a laptop and then forget
about.

## Can I contribute?

Yes — see [CONTRIBUTING.md](../CONTRIBUTING.md). Changes to `internal/sync` and
`internal/googleauth` get the most scrutiny, because those are the packages
where a mistake costs someone their calendar.
