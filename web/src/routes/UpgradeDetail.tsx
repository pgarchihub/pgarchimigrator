import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import type { UpgradeDetail as UpgradeDetailType, UpgradePhase } from "../lib/types";
import { parseDsnForDisplay } from "../lib/dsn";
import { formatDateTime } from "../lib/format";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { StatItem } from "../ui/StatItem";
import { TextField } from "../ui/TextField";

const POLL_INTERVAL_MS = 3000;

function phaseTone(phase: UpgradePhase): "success" | "coral" | "amber" | "neutral" {
  if (phase === "FAILED" || phase === "ABORTED") return "coral";
  if (phase === "READY") return "success";
  if (phase === "VALIDATING") return "amber";
  return "neutral";
}

function isTerminal(phase: UpgradePhase): boolean {
  return phase === "READY" || phase === "FAILED" || phase === "ABORTED";
}

// PHASE_ORDER gives every non-terminal phase a fixed position for the
// simple step indicator below — mirrors the ordering
// engines/postgresql/upgrade.Flow.Run itself drives the job through (see that
// function's own doc comment), not something this file invents
// independently.
const PHASE_ORDER: UpgradePhase[] = ["INTROSPECTING", "SCHEMA_CREATED", "SYNCING", "VALIDATING", "READY"];

function PhaseSteps({ current }: { current: UpgradePhase }) {
  const failed = current === "FAILED" || current === "ABORTED";
  const currentIndex = PHASE_ORDER.indexOf(current);

  return (
    <div className="flex items-center gap-1" aria-label="Upgrade progress">
      {PHASE_ORDER.map((phase, i) => {
        const done = !failed && currentIndex > i;
        const active = !failed && currentIndex === i;
        return (
          <div key={phase} className="flex items-center gap-1">
            <div
              className={
                "h-2 w-10 rounded-full " +
                (failed && i <= currentIndex
                  ? "bg-coral-400"
                  : done || active
                    ? "bg-petrol-500"
                    : "bg-ink-100")
              }
              title={phase}
            />
          </div>
        );
      })}
    </div>
  );
}

// RetryReviewConnection shows a connection string's own safe-to-display
// fields (host/port/username/database — NEVER the password, see
// parseDsnForDisplay's own doc comment) as part of the retry review
// panel below — the direct answer to "I don't know what values it's
// retrying."
function RetryReviewConnection({ label, dsn }: { label: string; dsn: string }) {
  const parsed = parseDsnForDisplay(dsn);
  if (!parsed) {
    return (
      <p className="text-xs text-ink-500">
        {label}: <span className="text-ink-400">(could not preview this connection)</span>
      </p>
    );
  }
  return (
    <p className="text-xs text-ink-500">
      <span className="font-medium text-ink-700">{label}:</span> {parsed.username}@{parsed.host}
      {parsed.port ? `:${parsed.port}` : ""} / {parsed.database}
    </p>
  );
}

export default function UpgradeDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [job, setJob] = useState<UpgradeDetailType | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [retrying, setRetrying] = useState(false);
  // showRetryReview gates the actual retry API call behind a visible
  // preview of exactly what will be reused — see confirmRetry's own
  // doc comment for why this exists: a retry silently reusing WRONG
  // stored values (a real case this was built for — a job whose
  // connection info never had a needed "Advanced" replication
  // override set) previously gave the user no way to notice before
  // it failed again the same way.
  const [showRetryReview, setShowRetryReview] = useState(false);
  // Editable override for the retry review panel's own replication
  // address fields — see confirmRetry's own comment for why these are
  // the ONLY editable part of a retry (everything else is reused
  // as-is): this is the one field genuinely likely to need correcting
  // between attempts, and the server rebuilds SourceReplicationRef from
  // these plus the ORIGINAL stored connection string, so the password
  // still never touches the browser.
  const [retryReplicationHost, setRetryReplicationHost] = useState("");
  const [retryReplicationPort, setRetryReplicationPort] = useState("");
  const pollRef = useRef<number | null>(null);

  const load = useCallback(async () => {
    if (!id) return;
    try {
      const result = await api.getUpgrade(id);
      setJob(result);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load this database migration.");
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  // Same "poll while not yet terminal" pattern as MigrationDetail's own
  // — an upgrade can genuinely run for hours, and this is the one
  // screen where live progress matters.
  useEffect(() => {
    if (!job || isTerminal(job.Phase)) {
      if (pollRef.current) window.clearInterval(pollRef.current);
      return;
    }
    pollRef.current = window.setInterval(load, POLL_INTERVAL_MS);
    return () => {
      if (pollRef.current) window.clearInterval(pollRef.current);
    };
  }, [job, load]);

  // confirmRetry starts a genuinely NEW upgrade job reusing the
  // original's own connection info entirely server-side — see
  // handleRetryUpgrade's own doc comment for why zero re-entry (not
  // even the source/target passwords) is the whole point. Only called
  // AFTER the user has seen the review panel below and explicitly
  // confirmed it — never directly from the initial "Retry" button
  // click.
  async function confirmRetry() {
    if (!id) return;
    setRetrying(true);
    try {
      const override =
        retryReplicationHost.trim() || retryReplicationPort.trim()
          ? {
              replicationHost: retryReplicationHost.trim() || undefined,
              replicationPort: retryReplicationPort.trim() || undefined,
            }
          : undefined;
      const result = await api.retryUpgrade(id, override);
      // Reset this screen's own state BEFORE navigating — since
      // react-router re-renders the SAME UpgradeDetail component
      // instance for a param change (it doesn't remount), leaving the
      // OLD failed job's state in place would otherwise flash the old
      // job's own FAILED banner/Retry button for a moment while the
      // new job's own data is still loading.
      setJob(null);
      setLoading(true);
      navigate(`/upgrades/${result.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Retry request failed.");
    } finally {
      // Always reset, success or failure — see this function's own
      // prior bug: never resetting on the success path left the button
      // stuck showing "Starting retry…" forever if navigation didn't
      // visually take effect for any reason.
      setRetrying(false);
      setShowRetryReview(false);
      setRetryReplicationHost("");
      setRetryReplicationPort("");
    }
  }

  if (loading && !job) {
    return <p className="text-sm text-ink-500">Loading…</p>;
  }

  if (error && !job) {
    return (
      <Card className="border-coral-200 bg-coral-50">
        <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
      </Card>
    );
  }

  if (!job) return null;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-center justify-between">
        <div>
          <Link to="/upgrades" className="text-xs text-ink-400 hover:text-petrol-700">
            ← All database migrations
          </Link>
          <h1 className="mt-1 font-mono text-lg font-medium text-ink-800">{job.ID}</h1>
        </div>
        <Badge tone={phaseTone(job.Phase)}>{job.Phase}</Badge>
      </div>

      {error && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-600">Live updates paused: {error}</div>
        </Card>
      )}

      {job.Phase === "FAILED" && job.LastError && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-700">
            <span className="font-medium">Failed:</span> {job.LastError}
          </div>
        </Card>
      )}

      {job.Phase === "FAILED" && !showRetryReview && (
        <div>
          <Button variant="secondary" onClick={() => setShowRetryReview(true)}>
            Retry this database migration
          </Button>
          <p className="mt-1.5 text-xs text-ink-400">
            Starts a new database migration with the same source, target, and schemas — nothing to re-enter.
          </p>
        </div>
      )}

      {showRetryReview && (
        <Card className="border-amber-200 bg-amber-50">
          <CardHeader>Review before retrying</CardHeader>
          <CardBody className="flex flex-col gap-3">
            <p className="text-xs text-ink-500">
              This will start a NEW database migration reusing exactly these stored details —
              passwords are never shown here, but the same ones on file will be reused.
            </p>
            <RetryReviewConnection label="Source" dsn={job.SourceConnectionRef} />
            <RetryReviewConnection label="Target" dsn={job.TargetConnectionRef} />
            {job.SourceReplicationRef ? (
              <RetryReviewConnection label="Source replication address" dsn={job.SourceReplicationRef} />
            ) : (
              <p className="text-xs text-ink-500">
                No replication address override on file — if the target's own server can&apos;t
                reach the source using the Source address above, this retry will fail the same way
                the original attempt did. Fix it below, or start a new database migration instead.
              </p>
            )}
            <div className="grid grid-cols-2 gap-3 rounded-lg border border-ink-100 bg-white p-3">
              <p className="col-span-2 text-xs font-medium text-ink-700">
                Set or correct the replication address for this retry (optional)
              </p>
              <div className="col-span-2 sm:col-span-1">
                <TextField
                  id="field-retry-replication-host"
                  label="Replication host"
                  placeholder="pg-logical"
                  value={retryReplicationHost}
                  onChange={(e) => setRetryReplicationHost(e.target.value)}
                />
              </div>
              <div className="col-span-2 sm:col-span-1">
                <TextField
                  id="field-retry-replication-port"
                  label="Replication port (optional)"
                  placeholder="5432"
                  inputMode="numeric"
                  value={retryReplicationPort}
                  onChange={(e) => setRetryReplicationPort(e.target.value)}
                />
              </div>
              <p className="col-span-2 text-xs text-ink-400">
                Reuses the Source connection&apos;s own username/password/database — the password
                is never sent to your browser, even here. Leave both blank to reuse the stored
                replication address (or lack of one) exactly as-is.
              </p>
            </div>
            <p className="text-xs text-ink-500">
              {job.Tables && job.Tables.length > 0
                ? `Scope: ${job.Tables.length} specific table(s) — ${job.Tables.map((t) => `${t.schema}.${t.table}`).join(", ")}`
                : job.Schemas && job.Schemas.length > 0
                  ? `Scope: schemas — ${job.Schemas.join(", ")}`
                  : "Scope: every schema on the source instance"}
            </p>
            <div className="flex gap-2 pt-1">
              <Button onClick={confirmRetry} disabled={retrying}>
                {retrying ? "Starting retry…" : "Confirm retry"}
              </Button>
              <Button
                variant="secondary"
                onClick={() => {
                  setShowRetryReview(false);
                  setRetryReplicationHost("");
                  setRetryReplicationPort("");
                }}
                disabled={retrying}
              >
                Cancel
              </Button>
            </div>
          </CardBody>
        </Card>
      )}

      {job.Phase === "READY" && (
        <Card className="border-petrol-200 bg-petrol-50">
          <div className="px-5 py-4 text-sm text-petrol-800">
            Every in-scope table is synced and verified. This tool does not perform cutover —
            repointing application traffic at the new instance is a separate, deliberate step.
          </div>
        </Card>
      )}

      <Card>
        <CardHeader>Progress</CardHeader>
        <CardBody>
          {!isTerminal(job.Phase) && <PhaseSteps current={job.Phase} />}
          <div className="mt-4 grid grid-cols-3 gap-4">
            <StatItem label="Tables total" value={String(job.TablesTotal)} />
            <StatItem label="Tables synced" value={String(job.TablesSynced)} />
            <StatItem label="Tables verified" value={String(job.TablesVerified)} />
          </div>
          <div className="mt-4 flex flex-wrap gap-4 border-t border-ink-100 pt-4 text-xs text-ink-400">
            <span>Started {formatDateTime(job.CreatedAt)}</span>
            <span>Updated {formatDateTime(job.UpdatedAt)}</span>
            {job.Schemas && job.Schemas.length > 0 && <span>Schemas: {job.Schemas.join(", ")}</span>}
          </div>
        </CardBody>
      </Card>

      <Card>
        <CardHeader>Tables</CardHeader>
        {job.tables && job.tables.length > 0 ? (
          <div className="overflow-x-auto">
            <table aria-label="Tables" className="w-full text-sm">
              <thead>
                <tr className="border-b border-ink-100 bg-ink-50 text-left text-xs uppercase tracking-wide text-ink-400">
                  <th className="px-5 py-3 font-medium">Table</th>
                  <th className="px-5 py-3 font-medium">Status</th>
                  <th className="px-5 py-3 font-medium">Rows synced</th>
                </tr>
              </thead>
              <tbody>
                {job.tables.map((table) => (
                  <tr
                    key={`${table.SchemaName}.${table.TableName}`}
                    className="border-b border-ink-50 last:border-0"
                  >
                    <td className="whitespace-nowrap px-5 py-3 font-mono text-ink-800">
                      {table.SchemaName}.{table.TableName}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3">
                      <Badge tone={phaseTone(table.Phase)}>{table.Phase}</Badge>
                      {table.LastError && <span className="ml-2 text-xs text-coral-600">{table.LastError}</span>}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3 font-mono text-ink-500">
                      {table.RowsSynced.toLocaleString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <CardBody>
            <p className="text-sm text-ink-500">
              {job.Phase === "INTROSPECTING"
                ? "Discovering tables on the source instance…"
                : "No tables recorded."}
            </p>
          </CardBody>
        )}
      </Card>
    </div>
  );
}
