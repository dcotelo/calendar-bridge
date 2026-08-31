# 4. The web UI binds loopback only, unconditionally

**Status:** Accepted

## Context

The web UI edits `config.yaml`. The config names arbitrary filesystem paths for
credentials and tokens. Anyone who can drive the UI can therefore point
calendar-bridge at any file the process can read, and cause a token to be written
anywhere it can write. It is a privileged local admin surface, not a dashboard.

It also serves **plaintext HTTP**. Terminating TLS in-process would mean
certificate handling, renewal, and a much larger attack surface, for a tool
whose entire premise is being small enough to audit.

The natural request is "let me reach it from my laptop", usually answered with
"bind 0.0.0.0 and set an auth token".

## Decision

`webui.New` **refuses to start** if `web_ui.listen_addr` is not a loopback
address. Not a warning — a hard refusal, regardless of whether an auth token is
configured.

To reach it remotely, forward the loopback port over SSH, or point a
TLS-terminating reverse proxy at it.

## Consequences

**Good.**

- The auth token and the config contents can never be sent over a network in the
  clear. The failure mode that a warning would permit is eliminated, not
  discouraged.
- The safe path (an SSH tunnel) is one command, and most people who would expose
  an admin port already have SSH to the host.
- Being unable to serve TLS is no longer a security decision the user can get
  wrong.

**Bad, and accepted.**

- It refuses a configuration some users genuinely want, and people will find that
  paternalistic. The error message explains why and gives the tunnel command,
  which is the least we can do.
- Reverse-proxy setups need the proxy on the same host, or a tunnel to it.
- "Loopback only" does **not** mean "only you" on a shared multi-user host —
  any local user can reach `127.0.0.1:8090`. That is what `web_ui.auth_token` is
  for, and why it exists despite the loopback restriction.

## Layered on top

Because loopback alone is not sufficient, the UI also has: a DNS-rebinding guard
rejecting non-loopback `Host` headers in the default no-token mode; CSRF checks
on every state-changing route; constant-time token comparison over SHA-256
digests; a strict CSP with a per-response nonce; and a 1 MiB body cap. See
[THREAT-MODEL.md](../THREAT-MODEL.md).

## Alternatives rejected

**Bind anywhere, require a token.** The token would travel in cleartext over the
network on every request. Self-defeating.

**Terminate TLS in-process.** Certificate provisioning and renewal, a much bigger
dependency surface, and a new set of ways to be misconfigured — for a page that
one person looks at occasionally.

**Warn instead of refuse.** Warnings are ignored. The whole point is that the
insecure configuration is not reachable.
