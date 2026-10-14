# Security Policy

Backyard is designed to run **inside a company's trust boundary**: every request is
authenticated by your identity provider (Okta/OIDC, Google IAP, AWS ALB, Cloudflare Access or
another trusted proxy) and every site can read and write the shared APIs. Please read
[docs/security.md](docs/security.md) for the threat model before deploying.

## Supported versions

Security fixes are released for the latest minor version. Run the newest release.

## Reporting a vulnerability

**Please do not open public issues for security problems.**

Report vulnerabilities privately through GitHub's
[security advisory form](https://github.com/thasami/backyard/security/advisories/new).
Include:

- a description of the issue and its impact,
- steps to reproduce (configuration, auth mode, storage/database backends),
- affected version(s) and, if possible, a suggested fix.

We aim to acknowledge reports within 3 business days and to ship a fix or mitigation within
30 days for confirmed high-severity issues. We will credit reporters in the release notes unless
you prefer to remain anonymous.

## Hardening checklist

- Never run `auth.mode: none` on a network reachable by people outside your organisation.
- Set a strong `server.secret` (`openssl rand -hex 32`) and keep it in a secret manager.
  Rotating it invalidates all sessions and API tokens.
- Restrict `server.trusted_proxies` to your load balancer ranges.
- Use `auth.allowed_domains` / `auth.allowed_emails` / `auth.allowed_groups` to limit who can
  sign in.
- Keep AI provider keys, database credentials and warehouse credentials server-side only.
- Use read-only credentials for warehouse sources.
- Enable `sites.ownership: owner` if teams must not overwrite each other's sites.
