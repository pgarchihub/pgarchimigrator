# Security Policy

## Supported Versions

pgArchiMigrator is early-stage software. Security fixes are made against
the `main` branch and the most recent tagged release. There is no
long-term support policy for older releases yet — if you're running an
older version, please upgrade to the latest release before reporting an
issue you suspect might already be fixed.

| Version | Supported |
| ------- | --------- |
| Latest release (`main`) | ✅ |
| Older releases | ❌ |

## Reporting a Vulnerability

**Please do not open a public GitHub issue for a suspected security
vulnerability.** This project touches production database schemas and
credentials directly (see [`internal/strategy`'s SQL-injection
validation](internal/strategy/validation.go) for an example of the kind
of issue that matters here) — a public issue gives anyone running this
tool a window to be exploited before a fix ships.

Instead, please use GitHub's private vulnerability reporting:

1. Go to the [Security tab](https://github.com/pgarchihub/pgarchimigrator/security) of this repository.
2. Click **"Report a vulnerability"**.
3. Describe the issue: what you found, how to reproduce it, and its
   potential impact (e.g. "arbitrary SQL execution via the
   `default_value` field", not just "there's a bug somewhere").

If you can't use GitHub's private reporting for some reason, open a
regular issue asking for an alternative contact method — without
including any vulnerability details in that issue itself.

### What to expect

This is currently maintained by a small team. We can't commit to a
fixed SLA, but as a general goal:

- **Acknowledgement**: within a few days of your report.
- **A fix or a mitigation plan**: as soon as reasonably possible,
  prioritized by severity — a real SQL injection or credential leak is
  treated as urgent; a denial-of-service requiring unusual local access
  is not.

We'll credit you in the release notes for the fix, unless you'd prefer
to stay anonymous — just let us know.

### Scope

Vulnerabilities we're specifically interested in:

- SQL injection anywhere DDL or user-supplied expressions are built
  (see `internal/strategy/validation.go`'s existing allow-list/blocklist
  approach for `DefaultValue`/`CheckExpression`/`ColumnType` — a gap in
  that same spirit elsewhere in the codebase is exactly the kind of
  thing worth reporting).
- Authentication/authorization bypass (role checks, session handling).
- Anything that lets a `viewer`-role account perform an `operator`- or
  `admin`-only action.
- Credential or connection-string leakage (logs, error messages, API
  responses).
- Dependency vulnerabilities that this project's own code path actually
  reaches (this project runs `govulncheck` in CI already — see
  `.github/workflows/ci.yml` — so a dependency vulnerability that
  `govulncheck` would already catch is lower priority than one it
  wouldn't).

Out of scope: vulnerabilities that require an attacker to already have
`admin` access to pgArchiMigrator itself, or superuser access to the
target PostgreSQL database — at that point they can already do
significant damage through means unrelated to this project.
