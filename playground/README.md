# pgArchiMigrator Community — Playground

One command, one real scenario: watch a zero-downtime `ALTER COLUMN
TYPE` (`orders.id`: `integer` → `bigint` — the single most common
real-world reason teams reach for this: running out of int32 room on
a primary key) run against a pre-seeded 5,000,000-row table.

## Requirements

Docker Desktop (Windows/Mac) or Docker Engine + Compose plugin
(Linux). Nothing else — no local build step, no manual signup.

## Running It

```bash
cd playground
docker compose up -d --wait
```

This pulls the real, published `ghcr.io/pgarchihub/pgarchimigrator`
image (the same one this repo's own main README's "Quick Start
(Docker)" section documents), seeds `orders` with 5,000,000 rows, and
creates an admin login automatically.

## Logging In

Open `http://localhost:8080`.

- **Email**: `admin@example.com`
- **Password**: `playground123`

## Watching the Migration

From the dashboard, start a new migration against the `orders` table:
**operation** `ALTER_COLUMN_TYPE`, **column** `id`, **new type**
`bigint`. Watch it move through PREFLIGHT → PREPARATION → SYNCING →
DELTA_SYNC → VALIDATING → SWAPPING in real time — `SWAPPING` reaching
`DONE` is the actual zero-downtime moment: `id` is genuinely `bigint`
now, live, with no table lock held for the bulk of that time.

## Timing

Real numbers, measured by this repository's own CI (GitHub Actions'
standard 2-vCPU runner — not a speed benchmark, a real measurement on
shared, modest hardware):

| Phase | Measured |
|---|---|
| `docker compose up` (image pull + the 5,000,000-row seed) | ~30s |
| Login → start migration → reach the zero-downtime swap | several minutes |

The migration itself is the slower part — `SHADOW_TABLE`'s own
Initial Sync copies the table in batches of 10,000 rows (not
configurable via this product's own API, only its source — see
`internal/engines/postgresql/shadowflow` in this repo), and 5,000,000
real rows is 500 real batches. This is deliberately kept at full
scale rather than shrunk to chase a faster number — the point of this
playground is seeing the mechanism work correctly on real data
volume, not a speed benchmark. Your own hardware, with more CPU/IOPS
than a shared CI runner, will very likely be faster.

## What's Rollback Window?

After the swap, the job enters a 10-minute `ROLLBACK_WINDOW` — a
deliberate safety period (not configurable) before the job reaches
its own terminal `COMPLETED` state and cleans up the now-unused old
table. `id` is already `bigint` and live during this entire window —
there's nothing to wait for to consider the demo "done."

## Starting Over

```bash
docker compose down -v
docker compose up -d --wait
```

(`-v` removes all data — the seeded table, the admin login,
everything — for a clean restart.)

## Troubleshooting

```bash
docker compose logs pg             # postgres + the seed script's own output
docker compose logs pgarchimigrator
docker compose logs bootstrap      # the one-shot admin-creation step
```

If `docker compose up --wait` times out, the seed may still be
running — `docker compose logs pg` will show whether
`/docker-entrypoint-initdb.d/init-seed.sql` has reached its own
`SELECT count(*) AS seeded_row_count FROM orders;` line yet.
