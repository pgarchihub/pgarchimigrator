import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import type { MigrationReport, StageView } from "../lib/types";
import { useAuth } from "../lib/auth";
import { formatBytes, formatDateTime, formatDuration, formatRowCount, isZeroTime } from "../lib/format";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { PhaseTrack } from "../ui/PhaseTrack";
import { StatItem } from "../ui/StatItem";

const POLL_INTERVAL_MS = 3000;

function strategyTone(strategy: string): "petrol" | "amber" | "neutral" {
  if (strategy === "SHADOW_TABLE") return "amber";
  if (strategy === "EXPAND_BACKFILL") return "petrol";
  return "neutral";
}

// lagTrendDisplay maps a ReplicationLagTrend value to a label/tone pair
// for the live lag indicator (see internal/api's lagTrendTracker for
// where this value comes from, and the load-testing story behind why it
// exists at all: a SHADOW_TABLE migration's delta sync can, under heavy
// write load, never converge — this makes that visible DURING the
// migration instead of only after a long, silent wait).
interface HealthCheck {
  label: string;
  status: "pass" | "warn";
  detail: string;
}

// computeHealthSummary distills everything else on this page into a
// handful of pass/warn checks a DBA can read in five seconds — see
// pgArchiMigrator_Guven_Katmani_Tasarimi.md's "3.6 — Sağlık Kartı" for
// the design intent. Purely a presentational aggregation of data already
// present in the Report (Stages, ResourceStatus) — no new backend work,
// no new API calls; every signal here is already fetched for the rest of
// this page anyway. Only called for a TERMINAL job — see this function's
// call site for why (a still-running job has no meaningful "did
// validation pass" or complete resource-cleanup answer yet: validation
// hasn't happened, and resources are SUPPOSED to still exist while
// actively in use).
// validatedSuccessDetail describes WHAT was actually confirmed by a
// successful VALIDATING stage — this varies by what the operation itself
// checks, so a single generic sentence would be misleading for anything
// other than SHADOW_TABLE (e.g. showing "row counts/checksums matched"
// for an ADD_INDEX migration, which never compared row counts at all —
// it confirmed the created index passed PostgreSQL's own validity
// check instead). See internal/progress's pipelineFor and
// internal/ddlflow's executeAddIndex/executeSetNotNull/
// executeExpandBackfill for what each operation's VALIDATING stage
// actually does.
function validatedSuccessDetail(job: MigrationReport): string {
  if (job.Strategy === "SHADOW_TABLE") {
    return "Row counts/checksums matched between source and shadow table";
  }
  if (job.Operation === "ADD_INDEX") {
    return "The created index passed PostgreSQL's own validity check";
  }
  if (job.Operation === "SET_NOT_NULL" || job.Operation === "ADD_CONSTRAINT") {
    return "The constraint was validated against every existing row";
  }
  // EXPAND_BACKFILL's two users: ADD_COLUMN (volatile default) and
  // RENAME_COLUMN — both confirm zero rows were left with an
  // incomplete/unsynced value after the backfill.
  return "No rows were left with an incomplete value after the backfill";
}

function computeHealthSummary(job: MigrationReport): HealthCheck[] {
  const checks: HealthCheck[] = [
    {
      label: "Outcome",
      status: job.Failed ? "warn" : "pass",
      detail: job.Failed ? "This migration did not complete successfully" : "Completed successfully",
    },
  ];

  const validatingStage = job.Stages.find((s) => s.Phase === "VALIDATING");
  if (validatingStage) {
    checks.push({
      label: "Data validated",
      status: validatingStage.Status === "DONE" ? "pass" : "warn",
      detail:
        validatingStage.Status === "DONE"
          ? validatedSuccessDetail(job)
          : "Validation did not complete — see the failure reason below",
    });
  }

  if (job.ResourceStatus && job.ResourceStatus.length > 0) {
    const lingering = job.ResourceStatus.filter((r) => r.exists);
    checks.push({
      label: "Resources cleaned up",
      status: lingering.length === 0 ? "pass" : "warn",
      detail:
        lingering.length === 0
          ? `All ${job.ResourceStatus.length} temporary resource(s) confirmed removed`
          : `${lingering.length} of ${job.ResourceStatus.length} resource(s) still present — see below`,
    });
  }

  return checks;
}

function lagTrendDisplay(trend: string): { label: string; tone: "coral" | "success" | "amber" | "neutral" } {
  switch (trend) {
    case "growing":
      // Deliberately just "Growing", not "may not converge" — that
      // stronger phrasing is reserved for the escalated warning below
      // (see ReplicationLagGrowingForSeconds), which only appears after
      // several minutes of UNBROKEN growth. A routine, momentary uptick
      // shouldn't share wording with a genuinely concerning, sustained
      // one — the two used to say the exact same thing, which made them
      // impossible to tell apart from the label alone.
      return { label: "Growing", tone: "amber" };
    case "shrinking":
      return { label: "Shrinking — catching up", tone: "success" };
    case "stable":
      return { label: "Stable", tone: "neutral" };
    default:
      return { label: "Measuring…", tone: "neutral" };
  }
}

// stepStatus computes the step list's label/color for a single stage —
// exported (and unit-tested) on its own because of a real, user-reported
// bug: a FAILED/ABORTED job's terminal stage is deliberately marked
// "CURRENT" by the backend (see internal/progress.Compute's early-return
// path), purely so PhaseTrack's graphic can highlight where things
// stopped. Reading that literally as "In progress" — which is what a
// naive stage.Status === "CURRENT" check does — is actively wrong
// wording for a job that has, in fact, stopped for good, not one still
// running.
export function stepStatus(
  stage: StageView,
  job: Pick<MigrationReport, "Failed" | "CurrentPhase">,
): { label: string; tone: "success" | "coral" | "amber" | "neutral" } {
  if (stage.Status === "DONE") {
    return { label: "Done", tone: "success" };
  }
  if (stage.Status === "CURRENT") {
    if (job.Failed) {
      return { label: "Failed", tone: "coral" };
    }
    if (job.CurrentPhase === "ABORTED") {
      return { label: "Aborted", tone: "amber" };
    }
    return { label: "In progress", tone: "amber" };
  }
  return { label: "Pending", tone: "neutral" };
}

// stepStatusIcon renders a small glyph to the left of a step's name in
// the STEPS list, matching stepStatus's own label — a checkmark for a
// completed step, an X for one that failed or was aborted (both are a
// form of "did not finish normally", grouped together visually), a
// spinning ring for the step genuinely in progress right now, and
// nothing for one that hasn't started (an empty circle would just be
// visual noise repeated once per pending step).
function stepStatusIcon(label: string): ReactNode {
  switch (label) {
    case "Done":
      return (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" className="shrink-0 text-petrol-600" aria-hidden="true">
          <circle cx="12" cy="12" r="10" fill="currentColor" />
          <path d="M7.5 12.5l3 3 6-6.5" stroke="white" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" fill="none" />
        </svg>
      );
    case "Failed":
    case "Aborted":
      return (
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" className="shrink-0 text-coral-600" aria-hidden="true">
          <circle cx="12" cy="12" r="10" fill="currentColor" />
          <path d="M8.5 8.5l7 7M15.5 8.5l-7 7" stroke="white" strokeWidth={2.2} strokeLinecap="round" />
        </svg>
      );
    case "In progress":
      return (
        <svg
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          className="shrink-0 text-amber-500 motion-safe:animate-spin"
          aria-hidden="true"
        >
          <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth={2.5} strokeOpacity={0.25} />
          <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" />
        </svg>
      );
    default:
      return null;
  }
}

export default function MigrationDetail() {
  const { id } = useParams<{ id: string }>();
  const { hasRole } = useAuth();
  const [job, setJob] = useState<MigrationReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [rollingBack, setRollingBack] = useState(false);
  // Opt-in — see MigrationReport.ImpactActiveQueries's own doc comment
  // for why this one indicator, unlike every other trust-layer signal on
  // this page, isn't just always on: the underlying query has real,
  // non-negligible cost, so it only runs while someone has explicitly
  // asked to see it.
  const [measureImpact, setMeasureImpact] = useState(false);
  const pollRef = useRef<number | null>(null);

  const load = useCallback(async () => {
    if (!id) return;
    try {
      const result = await api.getMigration(id, measureImpact);
      setJob(result);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load this migration.");
    } finally {
      setLoading(false);
    }
  }, [id, measureImpact]);

  useEffect(() => {
    load();
  }, [load]);

  // Poll while the job is still moving — a Preparation/Syncing/Rollback
  // Window phase can genuinely change between renders, and this is the
  // one screen where "is this still accurate right now" matters most.
  // Stops automatically once the job reaches a terminal phase.
  useEffect(() => {
    if (!job || job.Terminal) {
      if (pollRef.current) window.clearInterval(pollRef.current);
      return;
    }
    pollRef.current = window.setInterval(load, POLL_INTERVAL_MS);
    return () => {
      if (pollRef.current) window.clearInterval(pollRef.current);
    };
  }, [job, load]);

  async function handleRollback() {
    if (!id) return;
    if (!window.confirm("Roll back this migration? This cannot be undone.")) return;
    setRollingBack(true);
    try {
      const result = await api.rollbackMigration(id);
      setJob(result);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Rollback request failed.");
    } finally {
      setRollingBack(false);
    }
  }

  if (loading) {
    return <p className="text-sm text-ink-500">Loading migration…</p>;
  }

  if (error && !job) {
    return (
      <Card className="border-coral-200 bg-coral-50">
        <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
      </Card>
    );
  }

  if (!job) return null;

  // The frontend deliberately doesn't try to guess which operations allow
  // a post-completion rollback (some do — ADD_INDEX, SET_NOT_NULL,
  // ADD_CONSTRAINT, RENAME_COLUMN, DROP_INDEX all remain safely reversible
  // even after COMPLETED; others, like a plain ADD_COLUMN, refuse once
  // COMPLETED — see internal/ddlflow's Rollback for the real rules per
  // operation). The button is offered whenever the job isn't already
  // ABORTED; the backend is the actual source of truth and returns a
  // clear LastError via the 422 case (see api.rollbackMigration) if this
  // particular job can't be rolled back right now.
  const canOfferRollback = job.CurrentPhase !== "ABORTED" && hasRole("operator");
  const qualifiedTable = `${job.SchemaName}.${job.TableName}`;
  const hasName = job.Name.trim().length > 0;

  return (
    <div className="flex flex-col gap-6">
      <div>
        <Link to="/" className="text-sm text-ink-500 hover:text-petrol-700">
          ← All migrations
        </Link>
      </div>

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          {/* When the operator gave this migration a name, it becomes the
              headline and the table becomes a secondary label — otherwise
              the table itself is the headline, exactly as before this
              field existed. Either way nothing is lost: the qualified
              table name is always visible somewhere. */}
          <h1 className="break-all text-lg font-medium text-ink-800">{hasName ? job.Name : qualifiedTable}</h1>
          {hasName && <p className="break-all text-sm text-ink-500">{qualifiedTable}</p>}
          <p className="font-mono text-xs text-ink-400">{job.JobID}</p>
        </div>
        <div className="flex items-center gap-2">
          <Badge tone={strategyTone(job.Strategy)}>{job.Strategy}</Badge>
          <Badge
            tone={
              job.Failed ? "coral" : job.CurrentPhase === "ROLLBACK_WINDOW" ? "amber" : job.Terminal ? "success" : "neutral"
            }
          >
            {job.CurrentPhase}
          </Badge>
        </div>
      </div>

      {error && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
        </Card>
      )}

      {/* Only rendered for a TERMINAL job with something genuinely
          beyond the outcome badge to summarize (a VALIDATING stage
          and/or ResourceStatus present) — for the simplest, most common
          case (a quick DIRECT_DDL with neither), the phase badge above
          already says everything this card would, so it's skipped
          rather than showing a mostly-redundant single line. */}
      {job.Terminal &&
        (() => {
          const checks = computeHealthSummary(job);
          if (checks.length <= 1) return null;
          return (
            <Card>
              <CardHeader>
                <span className="text-sm font-medium text-ink-700">Health summary</span>
              </CardHeader>
              <CardBody className="flex flex-col gap-2">
                {checks.map((c) => (
                  <div key={c.label} className="flex items-start gap-2">
                    <span className={c.status === "pass" ? "text-petrol-600" : "text-coral-600"}>
                      {c.status === "pass" ? "✓" : "⚠"}
                    </span>
                    <div className="flex flex-col">
                      <span className="text-sm text-ink-700">{c.label}</span>
                      <span className="text-xs text-ink-400">{c.detail}</span>
                    </div>
                  </div>
                ))}
              </CardBody>
            </Card>
          );
        })()}

      {/* "What this migration does" — the whole reason this page used to
          feel bare: previously nothing here said what the job actually
          DOES beyond the strategy/phase badges. OperationSummary and
          Statements are computed server-side, straight from the job's own
          persisted parameters (see internal/progress.describeOperation),
          so this is accurate even for a job that finished hours ago. */}
      <Card>
        <CardHeader>
          <span className="text-sm font-medium text-ink-700">What this migration does</span>
        </CardHeader>
        <CardBody className="flex flex-col gap-4">
          <div className="flex items-center gap-2">
            <Badge tone="neutral">{job.Operation}</Badge>
            <p className="text-sm text-ink-700">{job.OperationSummary}</p>
          </div>

          {job.Description && (
            <p className="rounded-md bg-ink-50 px-3 py-2 text-sm text-ink-600">{job.Description}</p>
          )}

          {/* Optional chaining is deliberate defense-in-depth, not just
              style: the backend is now fixed to never send a null
              Statements field (see internal/progress.describeOperation),
              but this is exactly the field a real, user-reported crash
              came from — a Go nil slice serializing to JSON `null` and
              this unconditional `.length` access blowing up the whole
              screen. Guarding it here too means a FUTURE backend
              regression degrades gracefully (no SQL section shown)
              instead of crashing again. */}
          {job.Statements?.length > 0 && (
            <div>
              <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-ink-400">SQL</p>
              <div className="flex flex-col gap-1.5">
                {job.Statements.map((s, i) => (
                  <pre key={i} className="overflow-x-auto rounded-md bg-ink-900 px-3 py-2 font-mono text-xs text-ink-50">
                    {s}
                  </pre>
                ))}
              </div>
            </div>
          )}
        </CardBody>
      </Card>

      <Card>
        <CardHeader className="flex items-center justify-between">
          <span className="text-sm font-medium text-ink-700">Progress</span>
          <span className="font-mono text-sm text-ink-500">{Math.round(job.PercentComplete)}%</span>
        </CardHeader>
        <CardBody className="overflow-x-auto py-8">
          <PhaseTrack stages={job.Stages} />

          {/* Only present for a SHADOW_TABLE job with an active
              replication slot right now (see MigrationReport's own doc
              comment) — nothing to show for any other strategy, or once
              the migration is terminal. See internal/api's
              lagTrendTracker for where the trend value comes from, and
              the real load-testing story behind why this exists: a
              SHADOW_TABLE migration's delta sync can, under heavy write
              load, never converge — this makes that visible DURING the
              migration instead of only after a long, silent wait. */}
          {job.ReplicationLagBytes !== undefined && (
            <div className="mt-6 flex items-center justify-between rounded-md bg-ink-50 px-3 py-2">
              <span className="text-xs font-medium uppercase tracking-wide text-ink-400">
                Replication lag
              </span>
              <div className="flex items-center gap-2">
                <span className="font-mono text-sm text-ink-700">{formatBytes(job.ReplicationLagBytes)}</span>
                <Badge tone={lagTrendDisplay(job.ReplicationLagTrend ?? "unknown").tone}>
                  {lagTrendDisplay(job.ReplicationLagTrend ?? "unknown").label}
                </Badge>
              </div>
            </div>
          )}

          {/* Escalated, advisory-only warning — see
              ReplicationLagGrowingForSeconds's own doc comment for why
              this only appears after several minutes of UNBROKEN growth
              (not a momentary blip) and deliberately never triggers any
              automatic action: the decision to actually stop this
              migration is left to whoever's reading this, using the
              rollback control already available below, not made for
              them. Found necessary via a real load test where a
              SHADOW_TABLE migration's delta sync genuinely never
              converged under sustained heavy write load. */}
          {job.ReplicationLagGrowingForSeconds !== undefined && (
            <p className="mt-3 rounded-md bg-coral-50 px-3 py-2 text-xs text-coral-700">
              Replication lag has been growing for{" "}
              {formatDuration(job.ReplicationLagGrowingForSeconds * 1000)} without a break — this migration may not
              converge under the current write load. Consider stopping it and retrying during a quieter period.
            </p>
          )}

          {/* An external, environmental signal — PostgreSQL's own
              checkpoints being forced more often than scheduled, not
              something this migration itself did (see
              CheckpointPressureDetected's own doc comment for the real
              incident: checkpoints forced every 6-22 seconds under heavy
              write load, one taking 90 seconds, causing latency spikes
              that looked identical to an application bug from the
              outside). Shown as a note explaining a POSSIBLE cause for
              any latency you're seeing, not a claim that it's this
              migration's fault. */}
          {job.CheckpointPressureDetected && (
            <p className="mt-3 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700">
              PostgreSQL's checkpoints are currently being forced more often than scheduled — this can cause brief
              latency spikes unrelated to this migration itself. Consider reviewing{" "}
              <code className="font-mono">max_wal_size</code> if this happens often.
            </p>
          )}

          {/* Opt-in — see MigrationReport.ImpactActiveQueries's own doc
              comment for why this is the one indicator on this page
              that isn't just always on while the migration is running:
              the underlying query has real, non-negligible cost (a
              three-way join run on every poll), unlike the cheap
              system-view reads the other indicators use. Only offered
              while the migration is still running — once it's finished,
              there's nothing live left to measure, and any result
              already measured shows automatically below regardless. */}
          {!job.Terminal && (
            <label className="mt-3 flex items-center gap-2 text-xs text-ink-500">
              <input
                type="checkbox"
                checked={measureImpact}
                onChange={(e) => setMeasureImpact(e.target.checked)}
                className="rounded border-ink-300 text-petrol-600 focus:ring-petrol-500"
              />
              Measure this migration's impact on live query latency (adds a small extra query on each refresh)
            </label>
          )}
          {!job.Terminal && measureImpact && job.ImpactActiveQueries !== undefined && (
            <div className="mt-2 flex items-center justify-between rounded-md bg-ink-50 px-3 py-2">
              <span className="text-xs font-medium uppercase tracking-wide text-ink-400">
                Query impact on this table
              </span>
              <span className="font-mono text-sm text-ink-700">
                {job.ImpactActiveQueries} active ·{" "}
                {job.ImpactPeakQueryDurationSeconds !== undefined && (
                  <>peak {job.ImpactPeakQueryDurationSeconds.toFixed(2)}s</>
                )}
              </span>
            </div>
          )}
          {/* Automatic post-migration report — no checkbox needed here,
              unlike the live version above: this reads the DURABLE,
              already-persisted peak (see state.Job.ImpactPeakQueryDurationSeconds's
              own doc comment), which costs nothing extra to show since
              it's part of the job record already fetched for this page.
              Only appears if impact measurement was turned on for at
              least one poll WHILE this migration was running — if
              nobody ever checked the box above, there's genuinely
              nothing to report, and this stays hidden rather than
              showing a misleading "0". */}
          {job.Terminal && job.ImpactPeakQueryDurationSeconds !== undefined && (
            <div className="mt-3 flex items-center justify-between rounded-md bg-ink-50 px-3 py-2">
              <span className="text-xs font-medium uppercase tracking-wide text-ink-400">
                Peak query impact during this migration
              </span>
              <span className="font-mono text-sm text-ink-700">
                {job.ImpactPeakQueryDurationSeconds.toFixed(2)}s
              </span>
            </div>
          )}

          {/* Textual step list + duration, directly below the visual
              track — the graphic is fast to scan at a glance, but doesn't
              give a screen-reader user or someone wanting the exact
              phase names/status in words anything to read; this closes
              that gap without duplicating the graphic's information
              incorrectly (same Stages data, just rendered as text). */}
          {!isZeroTime(job.CreatedAt) && (
            <div className="mt-6 flex items-center justify-between border-t border-ink-100 pt-4">
              <span className="text-xs font-medium uppercase tracking-wide text-ink-400">Steps</span>
              <span className="font-mono text-xs text-ink-500">
                {job.Terminal
                  ? formatDuration(new Date(job.UpdatedAt).getTime() - new Date(job.CreatedAt).getTime())
                  : formatDuration(Date.now() - new Date(job.CreatedAt).getTime())}
              </span>
            </div>
          )}
          <ol className="mt-2 flex flex-col gap-1.5">
            {job.Stages.map((s) => {
              const { label, tone } = stepStatus(s, job);
              return (
                <li key={s.Phase} className="flex items-center justify-between text-sm">
                  <span className="flex items-center gap-2">
                    <span className="flex w-3.5 shrink-0 items-center justify-center">{stepStatusIcon(label)}</span>
                    <span className={s.Status === "PENDING" ? "text-ink-400" : "text-ink-700"}>
                      {s.Phase.replace(/_/g, " ")}
                    </span>
                  </span>
                  <Badge tone={tone}>{label}</Badge>
                </li>
              );
            })}
          </ol>
        </CardBody>
      </Card>

      {!isZeroTime(job.CreatedAt) && (
        <Card>
          <CardBody className="grid grid-cols-2 gap-6 sm:grid-cols-4">
            <StatItem label="Started" value={formatDateTime(job.CreatedAt)} />
            {job.Terminal ? (
              <>
                <StatItem label="Finished" value={formatDateTime(job.UpdatedAt)} />
                <StatItem
                  label="Duration"
                  value={formatDuration(new Date(job.UpdatedAt).getTime() - new Date(job.CreatedAt).getTime())}
                />
              </>
            ) : (
              <StatItem label="Elapsed" value={formatDuration(Date.now() - new Date(job.CreatedAt).getTime())} />
            )}
            {/* "Affected records": RowsProcessed is the real, exact count
                for operations that touch rows one at a time (a batched
                backfill); for everything else (a metadata-only DDL change
                like ADD_INDEX or a fixed-default ADD_COLUMN) there's no
                per-row processing to count, so EstimatedRowCount — the
                table's size at the moment this migration was created —
                is shown instead, honestly labeled as an estimate rather
                than implied to be an exact "rows touched" figure. */}
            {job.RowsProcessed > 0 ? (
              <StatItem
                label="Rows processed"
                value={
                  job.EstimatedRowCount > 0
                    ? `${formatRowCount(job.RowsProcessed)} / ~${formatRowCount(job.EstimatedRowCount)}`
                    : formatRowCount(job.RowsProcessed)
                }
              />
            ) : (
              job.EstimatedRowCount > 0 && (
                <StatItem label="Affected records (est.)" value={formatRowCount(job.EstimatedRowCount)} />
              )
            )}
          </CardBody>
        </Card>
      )}

      {/* Only present for a TERMINAL job (see MigrationReport's own doc
          comment) — a LIVE, directly-verified check of every transient
          resource this strategy might have created, not a log claiming
          what happened. Added specifically because of a real incident
          during this project's own development: an orphaned shadow
          table + a permanently mis-owned sequence sat invisible after a
          failed migration until manually found via psql — nothing in
          the product surfaced it. A DBA running this against a real
          production database shouldn't need direct database access just
          to confirm the tool actually cleaned up after itself. */}
      {job.ResourceStatus && job.ResourceStatus.length > 0 && (
        <Card>
          <CardHeader>
            <span className="text-sm font-medium text-ink-700">Resources</span>
          </CardHeader>
          <CardBody className="flex flex-col gap-2">
            <p className="text-xs text-ink-400">
              Checked directly against the database just now — not a record of what was supposed to happen.
            </p>
            {job.ResourceStatus.map((rs) => (
              <div key={rs.name} className="flex items-center justify-between gap-3 rounded-md bg-ink-50 px-3 py-2">
                <div className="flex min-w-0 flex-col">
                  <span className="text-sm text-ink-700">{rs.name}</span>
                  <span className="truncate font-mono text-xs text-ink-400">{rs.detail}</span>
                </div>
                <Badge tone={rs.exists ? "coral" : "success"}>{rs.exists ? "Still present" : "Cleaned up"}</Badge>
              </div>
            ))}
          </CardBody>
        </Card>
      )}

      {job.LastError && (
        <Card className="border-coral-200 bg-coral-50">
          <CardHeader className="border-coral-100">
            <span className="text-sm font-medium text-coral-700">
              {job.Failed ? "Failure reason" : "Last message"}
            </span>
          </CardHeader>
          <CardBody>
            <p className="font-mono text-sm text-coral-600">{job.LastError}</p>
          </CardBody>
        </Card>
      )}

      {job.CurrentPhase === "ROLLBACK_WINDOW" && (
        <Card className="border-amber-200 bg-amber-50">
          <CardBody className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium text-amber-600">Rollback window is open</p>
              <p className="text-sm text-amber-600/80">
                This migration will finalize automatically once the window closes.
              </p>
            </div>
          </CardBody>
        </Card>
      )}

      {canOfferRollback && (
        <div>
          <Button variant="danger" onClick={handleRollback} disabled={rollingBack}>
            {rollingBack ? "Rolling back…" : "Roll back this migration"}
          </Button>
        </div>
      )}
    </div>
  );
}
