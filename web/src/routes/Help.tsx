import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";

// PGARCHIMIGRATOR_REPO_URL is the project's designated GitHub
// repository — matches the module path declared in go.mod (module
// github.com/pgarchihub/pgarchimigrator).
const PGARCHIMIGRATOR_REPO_URL = "https://github.com/pgarchihub/pgarchimigrator";
const NEW_ISSUE_URL = `${PGARCHIMIGRATOR_REPO_URL}/issues/new`;
// pgArchiHub is the product family this tool belongs to (see
// docs/ecosystem/ARCHITECTURE.md) — ArchiOrbit Labs' own PostgreSQL
// product line. Replaces the earlier GitHub Sponsors link (see git
// history for that version) now that the product has a real product
// family/company identity to point to instead.
const PGARCHIHUB_URL = "https://pgarchihub.com";

const FEATURES_V1 = [
  "8 operation types — ADD_COLUMN, DROP_COLUMN, ALTER_COLUMN_TYPE, ADD_INDEX, DROP_INDEX, SET_NOT_NULL, ADD_CONSTRAINT, RENAME_COLUMN",
  "Automatic strategy selection — each operation is routed to the cheapest safe strategy (Direct DDL, Expand & Backfill, or Shadow Table) based on the operation and table size",
  "Dry-run previews — see the exact SQL, the chosen strategy, and any risk warnings before anything runs against your database",
  "Shadow table + logical replication for the hard cases — incompatible column type changes on large tables without blocking writes",
  "Live trust indicators while a migration runs — replication lag (escalating to an explicit warning if it's been growing without a break), checkpoint pressure, and an opt-in query-impact measurement",
  "Post-migration verification — confirms every temporary resource (shadow table, replication slot, publication) was actually cleaned up, and reports the validation result for every strategy, not just Shadow Table",
  "A one-glance Health summary on every completed migration's page — outcome, data validation, and resource cleanup, together",
  "Fleet-wide analytics on the dashboard — failure rate, average duration, and a per-strategy breakdown across every migration this instance has run",
  "Migration as Code — define migrations as version-controlled JSON files, apply them idempotently via the CLI, and get an automatic dry-run preview posted as a PR comment on every pull request that changes one",
  "Role-based access (viewer / operator / admin) and a rollback window after a shadow-table migration completes",
  "Supports PostgreSQL 12 through 18",
];

// FEATURES_V2 covers what shipped after v1 — schema-changing operations
// gained RENAME_TABLE/ADD_FOREIGN_KEY/ADD_GENERATED_COLUMN/PARTITION_TABLE
// (folded into the v1 list's own "8 operation types" line becoming 12,
// not repeated here as a separate bullet), plus two genuinely new areas:
// ArchiOrbit Labs ecosystem integration (mostly invisible day to day —
// event publishing, OAuth2 service accounts for automation — so not
// broken out bullet by bullet here) and the new whole-database migration
// capability with its own dashboard section.
const FEATURES_V2 = [
  "12 operation types now, up from 8 — added RENAME_TABLE, ADD_FOREIGN_KEY, ADD_GENERATED_COLUMN, and PARTITION_TABLE",
  "A second migration mode: move an entire database (schema AND data) to a different PostgreSQL instance — a different host, a different cloud, or a newer PostgreSQL version — via PostgreSQL's own native logical replication, with row-for-row validation before it's done",
  "Connect with plain Host/Port/Username/Password/Database fields — no need to type a raw connection string by hand",
  "Fetch a source's real schemas and tables and pick exactly what to include with checkboxes, instead of typing names from memory — and check what already exists on a target before you start, as a sanity check",
  "Retry a failed migration or database move with one click, with a review step showing exactly what will be reused before it starts — and, for a database move specifically, a way to fix the one thing most likely to need correcting (the replication address) without starting over from scratch",
  "Service-to-service automation support (OAuth2) for CI/CD and other tools to trigger migrations without a human clicking through the dashboard",
  "A redesigned dashboard layout — a collapsible sidebar, a fixed header with quick access to Help and your account, and a foreign-key picker that shows real tables/columns from your own database instead of asking you to type them from memory",
  "A one-line install script for Linux, macOS, and Windows — no need to configure Docker just to try this out",
];

const CHANGELOG_NOTE =
  "From this version onward, this build is distributed as the pgArchiMigrator Community Edition.";

export default function Help() {
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-lg font-medium text-ink-800">Help</h1>
        <p className="text-sm text-ink-500">About pgArchiMigrator, and where to get support.</p>
      </div>

      <Card>
        <CardHeader>
          <span className="text-sm font-medium text-ink-700">What this is</span>
        </CardHeader>
        <CardBody className="flex flex-col gap-4">
          <p className="text-sm text-ink-600">
            pgArchiMigrator performs zero-downtime schema changes on PostgreSQL. Point it at a table and the change
            you want, and it automatically picks the cheapest strategy that won&apos;t block reads or writes —
            simple changes run as fast, metadata-only DDL, while structural changes that would otherwise require a
            full table rewrite are handled through a shadow table kept in sync via logical replication, then swapped
            in atomically.
          </p>
        </CardBody>
      </Card>

      <Card>
        <CardHeader>
          <span className="text-sm font-medium text-ink-700">What&apos;s new</span>
        </CardHeader>
        <CardBody className="flex flex-col gap-5">
          <div>
            <div className="mb-2 flex items-center gap-2">
              <Badge tone="petrol">v2</Badge>
              <span className="text-sm font-medium text-ink-700">Current</span>
            </div>
            <ul className="flex flex-col gap-2">
              {FEATURES_V2.map((f) => (
                <li key={f} className="flex items-start gap-2 text-sm text-ink-600">
                  <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-petrol-500" aria-hidden="true" />
                  {f}
                </li>
              ))}
            </ul>
            <p className="mt-3 text-xs text-ink-400">{CHANGELOG_NOTE}</p>
          </div>

          <div className="border-t border-ink-100 pt-5">
            <div className="mb-2 flex items-center gap-2">
              <Badge tone="neutral">v1</Badge>
              <span className="text-sm font-medium text-ink-700">Original release</span>
            </div>
            <ul className="flex flex-col gap-2">
              {FEATURES_V1.map((f) => (
                <li key={f} className="flex items-start gap-2 text-sm text-ink-600">
                  <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-ink-300" aria-hidden="true" />
                  {f}
                </li>
              ))}
            </ul>
          </div>
        </CardBody>
      </Card>

      <Card>
        <CardHeader>
          <span className="text-sm font-medium text-ink-700">Source &amp; support</span>
        </CardHeader>
        <CardBody className="flex flex-col gap-3">
          <a
            href={PGARCHIMIGRATOR_REPO_URL}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-petrol-700 hover:text-petrol-800 hover:underline"
          >
            View the source on GitHub →
          </a>
          <a
            href={NEW_ISSUE_URL}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-petrol-700 hover:text-petrol-800 hover:underline"
          >
            Report a bug or suggest a feature →
          </a>
          <p className="text-sm text-ink-500">
            This product is part of the{" "}
            <a
              href={PGARCHIHUB_URL}
              target="_blank"
              rel="noreferrer"
              className="font-medium text-petrol-700 hover:text-petrol-800 hover:underline"
            >
              pgArchiHub
            </a>{" "}
            product family, developed by ArchiOrbit Labs.
          </p>
        </CardBody>
      </Card>
    </div>
  );
}
