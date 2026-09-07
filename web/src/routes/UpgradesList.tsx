import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import type { UpgradeJob, UpgradePhase } from "../lib/types";
import { useAuth } from "../lib/auth";
import { Button, buttonClasses } from "../ui/Button";
import { Card } from "../ui/Card";
import { Badge } from "../ui/Badge";

function phaseTone(phase: UpgradePhase): "success" | "coral" | "amber" | "neutral" {
  if (phase === "FAILED" || phase === "ABORTED") return "coral";
  if (phase === "READY") return "success";
  if (phase === "VALIDATING") return "amber";
  return "neutral";
}

function isTerminal(phase: UpgradePhase): boolean {
  return phase === "READY" || phase === "FAILED" || phase === "ABORTED";
}

export default function UpgradesList() {
  const { hasRole } = useAuth();
  const [jobs, setJobs] = useState<UpgradeJob[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setError(null);
    try {
      const result = await api.listUpgrades();
      setJobs(result);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not load upgrades.");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium text-ink-800">Database Migration</h1>
          <p className="text-sm text-ink-500">
            Whole-database PostgreSQL major-version upgrades — introspect, sync, and validate every
            in-scope table against a new instance. This tool never performs the actual cutover.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="secondary" onClick={load} disabled={loading}>
            Refresh
          </Button>
          {hasRole("admin") && (
            <Link to="/upgrades/new" className={buttonClasses("primary")}>
              Start database migration
            </Link>
          )}
        </div>
      </div>

      {error && (
        <Card className="border-coral-200 bg-coral-50">
          <div className="px-5 py-4 text-sm text-coral-600">{error}</div>
        </Card>
      )}

      {loading && !jobs && <p className="text-sm text-ink-500">Loading upgrades…</p>}

      {jobs && jobs.length === 0 && !loading && (
        <Card>
          <div className="flex flex-col items-center gap-2 px-5 py-16 text-center">
            <p className="text-sm font-medium text-ink-700">No upgrades yet</p>
            <p className="max-w-sm text-sm text-ink-500">
              Start one to sync an entire database to a new PostgreSQL instance, with live per-table
              progress.
            </p>
            {hasRole("admin") && (
              <Link to="/upgrades/new" className={buttonClasses("primary", "mt-2")}>
                Start database migration
              </Link>
            )}
          </div>
        </Card>
      )}

      {jobs && jobs.length > 0 && (
        <Card className="overflow-hidden">
          <div className="overflow-x-auto">
            <table aria-label="Database Migration" className="w-full text-sm">
              <thead>
                <tr className="border-b border-ink-100 bg-ink-50 text-left text-xs uppercase tracking-wide text-ink-400">
                  <th className="px-5 py-3 font-medium">Job</th>
                  <th className="px-5 py-3 font-medium">Status</th>
                  <th className="px-5 py-3 font-medium">Tables verified</th>
                  <th className="px-5 py-3 font-medium"></th>
                </tr>
              </thead>
              <tbody>
                {jobs.map((job) => (
                  <tr key={job.ID} className="border-b border-ink-50 last:border-0 hover:bg-ink-50/60">
                    <td className="whitespace-nowrap px-5 py-3">
                      <Link to={`/upgrades/${job.ID}`} className="font-mono text-xs text-ink-800 hover:text-petrol-700">
                        {job.ID}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-5 py-3">
                      <Badge tone={phaseTone(job.Phase)}>{job.Phase}</Badge>
                      {!isTerminal(job.Phase) && (
                        <span className="ml-2 text-xs text-ink-400">still running</span>
                      )}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3 font-mono text-ink-500">
                      {job.TablesVerified} / {job.TablesTotal}
                    </td>
                    <td className="whitespace-nowrap px-5 py-3 text-right">
                      <Link
                        to={`/upgrades/${job.ID}`}
                        className="text-sm font-medium text-petrol-700 hover:text-petrol-800"
                      >
                        Details
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}
