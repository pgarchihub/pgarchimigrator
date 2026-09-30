-- Seeds a realistic "orders" table with 5,000,000 rows for the
-- playground's own zero-downtime ALTER COLUMN TYPE demo (id:
-- integer -> bigint — the single most common real-world reason teams
-- reach for a zero-downtime type change: running out of int32 room on
-- a primary key). Runs automatically via postgres's own
-- /docker-entrypoint-initdb.d mechanism on first container start —
-- see playground/docker-compose.yml's own comment for why this table
-- stays a normal, durable (logged) table rather than UNLOGGED: this
-- is meant to look like a real production table, not a
-- seeding-only shortcut that would misrepresent the demo.

-- synchronous_commit is OFF for this session only (initial data load,
-- not the migration itself) — safe here since this is throwaway demo
-- data, not something the playground promises durability guarantees
-- for during seeding; speeds up the bulk INSERT without changing the
-- table's own LOGGED status (crash-safety for the table itself is
-- unaffected once seeding completes).
SET synchronous_commit = off;

CREATE TABLE orders (
    id            integer PRIMARY KEY,
    customer_name text NOT NULL,
    amount        numeric(10, 2) NOT NULL,
    status        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A single bulk INSERT ... SELECT FROM generate_series is
-- dramatically faster than 5,000,000 individual row insertions (no
-- per-statement round-trip/parse overhead) — this is the whole
-- seeding step, deliberately kept to one statement.
INSERT INTO orders (id, customer_name, amount, status, created_at)
SELECT
    i,
    'customer_' || (i % 50000),
    (random() * 1000)::numeric(10, 2),
    (ARRAY['pending', 'paid', 'shipped', 'delivered', 'refunded'])[1 + (i % 5)],
    now() - (random() * interval '365 days')
FROM generate_series(1, 5000000) AS i;

ANALYZE orders;
