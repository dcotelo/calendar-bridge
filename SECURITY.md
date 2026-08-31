# Security Policy

## Supported versions

calendar-bridge is pre-1.0 and does not yet maintain multiple supported
release branches. Security fixes land on `main`; there is no backport
policy at this stage.

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Report privately via [GitHub Security Advisories](https://github.com/dcotelo/calendar-bridge/security/advisories/new)
for this repository. If that's unavailable, email <me@dcotelo.dev> with
"calendar-bridge security" in the subject.

Please include:

- A description of the vulnerability and its impact.
- Steps to reproduce, or a proof of concept if you have one.
- The version/commit you tested against.

You should get an acknowledgment within a few days. This is a small
side project maintained by one person, not a funded security team — please
be patient, but a genuine credential-handling or data-integrity bug will be
prioritized immediately.

## Scope

Given what calendar-bridge does, the most relevant vulnerability classes are:

- **Credential handling** — anything that could leak an OAuth token, client
  secret, credentials file path, or the webhook verification token
  (`internal/googleauth`, `internal/webhook`). Note: calendar-bridge warns at
  startup if a credentials or token file is group/world-readable; report any
  path where a secret is logged, transmitted, or persisted with loose
  permissions.
- **Data integrity / cross-account leakage** — anything that could cause
  calendar-bridge to overwrite, delete, or expose a real user event it
  doesn't own, or leak event content across account boundaries when only
  free/busy state should propagate (`internal/sync`, especially the
  owned-block tagging invariant — see `.coderabbit.yaml` path instructions
  for the exact invariant).
- **Supply chain** — a malicious or compromised dependency; see
  [Dependabot](.github/dependabot.yml) and the `govulncheck` CI job for
  what's already automated here.

Out of scope: vulnerabilities in Google's own Calendar API, OAuth
infrastructure, or Google Cloud Console — report those to Google directly.

## Disclosure

Once a fix is available, a GitHub Security Advisory will be published
crediting the reporter (unless you prefer to stay anonymous).
