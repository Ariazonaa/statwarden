# Security policy

Statwarden stores credentials for the systems it watches, sends alerts on your
behalf and is often reachable from the internet. Security reports are taken
seriously and answered before anything else.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.** Report it
privately through GitHub:

> **[Report a vulnerability](https://github.com/Ariazonaa/statwarden/security/advisories/new)**
> (repository → Security → Advisories → "Report a vulnerability")

Only the maintainer sees the report, and the fix can be prepared in a private
fork before anything is published.

Include what you can of:

- the affected version or commit (`/api/v1/version` shows both),
- the component (API endpoint, monitor type, notifier, status page, …),
- steps to reproduce or a proof of concept,
- the impact you see — what an attacker gains, and with which role, if any.

You will get an acknowledgement within **5 working days**. Once the issue is
confirmed, a fix is prioritised over feature work; you will hear when it is
released and, if you want, be credited in the release notes.

Please give a reasonable window to fix the problem before disclosing it — 90
days, or sooner by agreement once a fixed release is out.

## Supported versions

Only the latest release and `main` receive security fixes. There are no
long-term support branches.

## Scope

In scope: everything in this repository — the server, the embedded web UI,
`statwarden-cli`, the Dockerfile and the shipped configuration examples.

Out of scope: vulnerabilities in a dependency that are not reachable from
Statwarden's code (report those upstream), findings that need an already
compromised admin account or host, and missing hardening headers on endpoints
that serve no sensitive data.

## What is already in place

So you know what a report is measured against:

- Secrets at rest (notification credentials, monitor passwords, webhook secrets)
  are sealed with AES-256-GCM under `ENCRYPTION_KEY`.
- Passwords are argon2id; sessions are opaque server-side tokens; CSRF is
  double-submit; login is rate-limited with account lockout; TOTP and scoped,
  expiring API keys are supported.
- Every push and pull request runs gitleaks over the full history, Trivy over
  the tree, `npm audit` and `govulncheck`; the published image is scanned with
  Trivy before it is pushed. A finding at HIGH or above fails the build.
