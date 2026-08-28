# Contributing

pgArchiMigrator is early-stage — this guide is intentionally short. If
something here is missing or unclear, that's useful feedback too; open
an issue about it.

## Before you write code

For anything beyond a small fix (a typo, an obviously-correct bug fix),
please open an issue first — either a [bug
report](.github/ISSUE_TEMPLATE/bug_report.yml) or a [feature
request](.github/ISSUE_TEMPLATE/feature_request.yml) — describing what
you want to change and why. This project makes deliberate, sometimes
non-obvious trade-offs (see e.g. `internal/strategy`'s
`validStrategiesByOperation` or `internal/api`'s `impactTracker` for
examples with real incidents behind them) — a short discussion up front
can save you from building something that conflicts with one of those,
or that's already been tried and reverted for a reason that isn't
obvious from the code alone.

## Running this locally

See the root [`README.md`](README.md) for setup, and
[`docs/migration-as-code.md`](docs/migration-as-code.md) if your change
touches migration files specifically. In short:

```bash
docker compose -f deploy/docker-compose.dev.yml up -d
go test ./... -v                                      # unit tests
go test ./... -tags=integration -timeout 5m -v -p 1   # integration tests — see the -p 1 note below
cd web && npm install && npm test                      # frontend
```

**The `-p 1` flag on integration tests is not optional.** Every
integration test package shares one real PostgreSQL instance, and
several packages deliberately create real locks/transactions as part of
their own testing. Without `-p 1`, Go's default cross-package test
parallelism can make unrelated packages race against each other on that
shared database — this caused a real, confirmed CI failure once (see
`.github/workflows/ci.yml`'s own comment on this).

## Code style

- Go: `gofmt` is enforced in CI — run `gofmt -w .` before committing.
- No linter config beyond that yet; use your judgment and match the
  surrounding code's style.
- Comments in this codebase tend to explain *why*, not just *what* —
  especially for anything that looks like it could be simplified but
  isn't. If you're changing something and the "why" isn't already
  documented, that's worth asking about before assuming it's dead code.

## Tests

New behavior needs a test. If you're fixing a bug, a regression test
that fails without your fix and passes with it is the most convincing
way to demonstrate the fix actually works — several existing tests in
this codebase are written exactly that way, with a comment explaining
the real incident behind them.

## Security issues

Please don't open a public issue for a suspected vulnerability — see
[`SECURITY.md`](SECURITY.md) for private reporting instructions.

## License

By contributing, you agree that your contributions will be licensed
under this project's [Apache License 2.0](LICENSE).
