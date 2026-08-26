import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import type { Analytics, MigrationReport } from "../lib/types";
import { useAuth } from "../lib/auth";
import { formatDuration, formatRowCount } from "../lib/format";
import { Button, buttonClasses } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { PhaseTrack } from "../ui/PhaseTrack";
import { StatItem } from "../ui/StatItem";

function strategyTone(strategy: string): "petrol" | "amber" | "neutral" {
  if (strategy === "SHADOW_TABLE") return "amber";
  if (strategy === "EXPAND_BACKFILL") return "petrol";
  return "neutral";
}

function phaseTone(job: MigrationReport): "success" | "coral" | "amber" | "neutral" {
  if (job.Failed) return "coral";
  if (job.CurrentPhase === "ROLLBACK_WINDOW") return "amber";
  if (job.Terminal) return "success";
  return "neutral";
}

// A small inline chevron rather than pulling in an icon font/library for
// one glyph — matches this app's existing "no icon dependency" footprint
// (see the rest of ./ui, which is all text/SVG-shape based already).
function DetailsIcon() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M9 18l6-6-6-6" />
    </svg>
  );
}

export type StatusFilter = "" | "IN_PROGRESS" | "ROLLBACK_WINDOW" | "COMPLETED" | "FAILED" | "ABORTED";

// jobMatchesStatusFilter groups the many possible CurrentPhase values into
// the handful of states an operator actually thinks in terms of when
// scanning a list — exported (and unit-tested) on its own since the
// COMPLETED/IN_PROGRESS distinction in particular depends on combining
// Terminal + Failed + CurrentPhase correctly, not just a single field.
export function jobMatchesStatusFilter(job: MigrationReport, filter: StatusFilter): boolean {
  switch (filter) {
    case "":
      return true;
    case "ROLLBACK_WINDOW":
      return job.CurrentPhase === "ROLLBACK_WINDOW";
    case "COMPLETED":
      return job.Terminal && !job.Failed && job.CurrentPhase === "COMPLETED";
    case "FAILED":
      return job.Failed;
    case "ABORTED":
      return job.CurrentPhase === "ABORTED";
    case "IN_PROGRESS":
      // Everything still moving that isn't specifically sitting in its
      // rollback window — that state gets its own filter option above
      // since it's operationally distinct (a job an operator might need
      // to act on) rather than just "still running".
      return !job.Terminal && job.CurrentPhase !== "ROLLBACK_WINDOW";
  }
}

// jobMatchesDateRange checks CreatedAt against an optional [from, to]
// range — either bound may be empty to mean "unbounded on that side".
// `to` is treated as inclusive of the whole day (23:59:59.999), matching
// what a person picking a date in a <input type="date"> would expect
// ("migrations created ON this date" should still match, not silently
// exclude everything from that day because of the time-of-day cutoff).
export function jobMatchesDateRange(job: MigrationReport, from: string, to: string): boolean {
  if (!from && !to) return true;
  const created = new Date(job.CreatedAt).getTime();
  if (Number.isNaN(created)) return true; // never filter out a job over an unparseable timestamp
  if (from) {
    const fromMs = new Date(from).getTime();
    if (!Number.isNaN(fromMs) && created < fromMs) return false;
  }
  if (to) {
    const toDate = new Date(to);
    if (!Number.isNaN(toDate.getTime())) {
      // setUTCHours, not setHours: new Date("2026-08-20") (a bare
      // date-only ISO string) always parses as UTC midnight, but
      // setHours() operates in the machine's LOCAL timezone — mixing the
      // two silently shifts the "end of day" boundary by the runner's
      // UTC offset. A real bug found exactly this way: this test passed
      // in one timezone and failed in another (Istanbul, UTC+3) for the
      // identical input, because setHours(23,59,59,999) there produced
      // 20:59:59.999Z, not 23:59:59.999Z. job.CreatedAt from the backend
      // is always UTC ("Z" suffix), so the boundary must be computed in
      // UTC too, not local time, to compare correctly regardless of
      // which timezone this code happens to run in.
      toDate.setUTCHours(23, 59, 59, 999);
      if (created > toDate.getTime()) return false;
    }
  }
  return true;
}

const filterSelectClasses =
  "rounded-md border border-ink-200 px-2.5 py-1.5 text-sm text-ink-800 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500";

export default function Dashboard() {
  const { hasRole } = useAuth();
  const [jobs, setJobs] = useState<MigrationReport[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [analytics, setAnalytics] = useState<Analytics | null>(null);

  const [dateFrom, setDateFrom] = useState("");
  const [dateTo, setDateTo] = useState("");
  const [strategyFilter, setStrategyFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("");

  const load = useCallback(async () => {
    setError(null);
    try {
      const result = await api.listMigrations();
      setJobs(result);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load migrations.");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    // Best-effort, separate from the main jobs load — a failure here
    // shouldn't block the migrations list itself from rendering (see
    // the render logic below: the analytics card simply doesn't appear
    // if this fails or is still loading, rather than surfacing its own
    // error banner for what's a secondary, summary view).
    api.getAnalytics().then(setAnalytics, () => {});
  }, [load]);

  const filtersActive = Boolean(dateFrom || dateTo || strategyFilter || statusFilter);

  const filteredJobs = useMemo(() => {
    if (!jobs) return null;
    return jobs.filter(
      (job) =>
        jobMatchesDateRange(job, dateFrom, dateTo) &&
        (!strategyFilter || job.Strategy === strategyFilter) &&
        jobMatchesStatusFilter(job, statusFilter),
    );
  }, [jobs, dateFrom, dateTo, strategyFilter, statusFilter]);

  function clearFilters() {
    setDateFrom("");
    setDateTo("");
    setStrategyFilter("");
    setStatusFilter("");
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium text-ink-800">Migrations</h1>
          <p className="text-sm text-ink-500">Every schema change tracked from start to rollback window.</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="secondary" onClick={load} disabled={loading}>
            Refresh
          </Button>
          {hasRole("operator") && (
            <Link to="/new" className={buttonClasses("primary")}>
              New migration
            </Link>
          )}
        </div>
      </div>

      {/* Only rendered once loaded, and only when there's genuinely
          something to summarize (at least one TERMINAL migration) —
          computed entirely from existing job records server-side (see
          progress.ComputeAnalytics's own doc comment), no new database
          queries against the target PostgreSQL server. A quiet,
          brand-new deployment with nothing terminal yet just skips this
          card rather than showing an all-zeroes summary that wouldn't
          mean anything. */}
      {analytics && analytics.terminalMigrations > 0 && (
        <Card>
          <CardHeader>
            <span className="text-sm font-medium text-ink-700">Fleet summary</span>
          </CardHeader>
          <CardBody>
            <div className="flex flex-wrap gap-6">
              <StatItem label="Terminal migrations" value={formatRowCount(analytics.terminalMigrations)} />
              <StatItem label="Failure rate" value={`${(analytics.failureRate * 100).toFixed(1)}%`} />
              <StatItem label="Average duration" value={formatDuration(analytics.averageDurationMs)} />
            </div>
            <div className="mt-4 flex flex-col gap-1.5 border-t border-ink-100 pt-4">
              <span className="text-xs font-medium uppercase tracking-wide text-ink-400">By strategy</span>
              {Object.entries(analytics.strategyBreakdown)
                .sort(([, a], [, b]) => b.count - a.count)
                .map(([strategy, stats]) => (
                  <div key={strategy} className="flex items-center justify-between text-sm">
                    <span className="text-ink-600">{strategy}</span>
                    <span className="font-mono text-ink-500">
                      {stats.count} · {(stats.failureRate * 100).toFixed(1)}% failed · avg{" "}
                      {formatDuration(stats.averageDurationMs)}
                    </span>
                  </div>
                ))}
            </div>
          </CardBody>
        </Card>
      )}

      {jobs && jobs.length > 0 && (
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-ink-500">From</span>
            <input
              type="date"
              value={dateFrom}
              onChange={(e) => setDateFrom(e.target.value)}
              className={filterSelectClasses}
            />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-ink-500">To</span>
            <input
              type="date"
              value={dateTo}
              onChange={(e) => setDateTo(e.target.value)}
              className={filterSelectClasses}
            />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-ink-500">Strategy</span>
            <select
              value={strategyFilter}
              onChange={(e) => setStrategyFilter(e.target.value)}
              className={filterSelectClasses}
            >
              <option value="">All</option>
              <option value="DIRECT_DDL">DIRECT_DDL</option>
              <option value="EXPAND_BACKFILL">EXPAND_BACKFILL</option>
              <option value="SHADOW_TABLE">SHADOW_TABLE</option>
            </select>
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-ink-500">Status</span>
            <select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}
              className={filterSelectClasses}
            >
              <option value="">All</option>
              <option value="IN_PROGRESS">In progress</option>
              <option value="ROLLBACK_WINDOW">Rollback window</option>
              <option value="COMPLETED">Completed</option>
              <option value="FAILED">Failed</option>
              <option value="ABORTED">Aborted</option>
            </select>
          </label>
          {filtersActive && (
            <Button variant="ghost" onClick={clearFilters}>
              Clear filters
            </Button>
          )}
          {filtersActive && filteredJobs && (
            <span className="ml-auto self-center text-xs text-ink-400">
              Showing {filteredJobs.length} of {jobs.length}
            </span>
          )}
        </div>
      )}

      {error && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
        </Card>
      )}

      {loading && !jobs && <p className="text-sm text-ink-500">Loading migrations…</p>}

      {jobs && jobs.length === 0 && !loading && (
        <Card>
          <div className="flex flex-col items-center gap-2 px-5 py-16 text-center">
            <p className="text-sm font-medium text-ink-700">No migrations yet</p>
            <p className="max-w-sm text-sm text-ink-500">
              Start one to see its strategy, live SQL preview, and phase progress here.
            </p>
            {hasRole("operator") && (
              <Link to="/new" className={buttonClasses("primary", "mt-2")}>
                New migration
              </Link>
            )}
          </div>
        </Card>
      )}

      {jobs && jobs.length > 0 && filteredJobs && filteredJobs.length === 0 && (
        <Card>
          <div className="flex flex-col items-center gap-2 px-5 py-16 text-center">
            <p className="text-sm font-medium text-ink-700">No migrations match these filters</p>
            <Button variant="secondary" onClick={clearFilters} className="mt-2">
              Clear filters
            </Button>
          </div>
        </Card>
      )}

      {filteredJobs && filteredJobs.length > 0 && (
        <Card className="overflow-hidden">
          <div className="overflow-x-auto">
            <table aria-label="Migrations" className="w-full text-sm">
              <thead>
                <tr className="border-b border-ink-100 bg-ink-50 text-left text-xs uppercase tracking-wide text-ink-400">
                  <th className="px-5 py-3 font-medium">Table</th>
                  <th className="px-5 py-3 font-medium">Strategy</th>
                  <th className="px-5 py-3 font-medium">Progress</th>
                  <th className="px-5 py-3 font-medium">Status</th>
                  <th className="px-5 py-3 font-medium">Duration</th>
                  <th className="px-5 py-3 font-medium"></th>
                </tr>
              </thead>
              <tbody>
                {filteredJobs.map((job) => {
                  const qualifiedTable = `${job.SchemaName}.${job.TableName}`;
                  const hasName = job.Name.trim().length > 0;
                  return (
                    <tr key={job.JobID} className="border-b border-ink-50 last:border-0 hover:bg-ink-50/60">
                      <td className="whitespace-nowrap px-5 py-3">
                        <Link
                          to={`/migrations/${job.JobID}`}
                          className="font-medium text-ink-800 hover:text-petrol-700"
                        >
                          {hasName ? job.Name : qualifiedTable}
                        </Link>
                        {/* When the job has a name, the qualified table
                            stays visible as a secondary line — otherwise
                            it's already shown above as the link text. */}
                        <div className="font-mono text-xs text-ink-400">
                          {hasName ? qualifiedTable : job.JobID}
                        </div>
                      </td>
                      <td className="whitespace-nowrap px-5 py-3">
                        <Badge tone={strategyTone(job.Strategy)}>{job.Strategy}</Badge>
                      </td>
                      <td className="px-5 py-3">
                        <div className="w-40">
                          <PhaseTrack stages={job.Stages} compact />
                        </div>
                      </td>
                      <td className="whitespace-nowrap px-5 py-3">
                        <Badge tone={phaseTone(job)}>{job.CurrentPhase}</Badge>
                      </td>
                      <td className="whitespace-nowrap px-5 py-3 font-mono text-ink-500">
                        {formatDuration(
                          (job.Terminal ? new Date(job.UpdatedAt).getTime() : Date.now()) -
                            new Date(job.CreatedAt).getTime(),
                        )}
                      </td>
                      <td className="whitespace-nowrap px-5 py-3 text-right">
                        <Link
                          to={`/migrations/${job.JobID}`}
                          aria-label={`View details for ${hasName ? job.Name : qualifiedTable}`}
                          className="inline-flex items-center justify-center rounded-md p-1.5 text-ink-400 transition-colors hover:bg-ink-100 hover:text-petrol-700"
                        >
                          <DetailsIcon />
                        </Link>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}
