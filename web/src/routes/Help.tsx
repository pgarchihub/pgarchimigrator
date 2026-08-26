import { Card, CardBody, CardHeader } from "../ui/Card";

// PGARCHIMIGRATOR_REPO_URL is the project's designated GitHub
// repository — matches the module path declared in go.mod (module
// github.com/pgarchihub/pgarchimigrator).
const PGARCHIMIGRATOR_REPO_URL = "https://github.com/pgarchihub/pgarchimigrator";
const NEW_ISSUE_URL = `${PGARCHIMIGRATOR_REPO_URL}/issues/new`;
// GitHub's own Sponsors mechanism — see .github/FUNDING.yml, which is
// what makes the "Sponsor" button appear on the repository page itself;
// this is just a direct link to the same destination for anyone reading
// the in-app Help page instead.
const SPONSOR_URL = "https://github.com/sponsors/pgarchihub";

const FEATURES = [
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
          <ul className="flex flex-col gap-2">
            {FEATURES.map((f) => (
              <li key={f} className="flex items-start gap-2 text-sm text-ink-600">
                <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-petrol-500" aria-hidden="true" />
                {f}
              </li>
            ))}
          </ul>
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
          <a
            href={SPONSOR_URL}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-petrol-700 hover:text-petrol-800 hover:underline"
          >
            Sponsor this project on GitHub →
          </a>
        </CardBody>
      </Card>
    </div>
  );
}
