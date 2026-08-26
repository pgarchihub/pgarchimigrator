import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    getMigration: vi.fn(),
    rollbackMigration: vi.fn(),
    setupRequired: vi.fn().mockResolvedValue({ required: false }),
    getVersion: vi.fn().mockResolvedValue({ version: "test" }),
  },
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
      this.name = "ApiError";
    }
  },
  setUnauthorizedHandler: vi.fn(),
}));

import { api } from "../lib/api";
import type { MigrationReport } from "../lib/types";
import { AuthProvider } from "../lib/auth";
import MigrationDetail, { stepStatus } from "./MigrationDetail";

function makeJob(overrides: Record<string, unknown> = {}): MigrationReport {
  return {
    JobID: "job-1",
    SchemaName: "public",
    TableName: "orders",
    Strategy: "DIRECT_DDL",
    CurrentPhase: "COMPLETED",
    Stages: [{ Phase: "PREPARATION", Status: "DONE" }],
    PercentComplete: 100,
    Terminal: true,
    Failed: false,
    LastError: "",
    CreatedAt: "2026-08-20T10:00:00Z",
    UpdatedAt: "2026-08-20T10:01:00Z",
    EstimatedRowCount: 0,
    RowsProcessed: 0,
    Name: "",
    Description: "",
    Operation: "ADD_COLUMN",
    OperationSummary: 'Added column "status" (text) to public.orders',
    Statements: [`ALTER TABLE "public"."orders" ADD COLUMN "status" text`],
    ...overrides,
  } as unknown as MigrationReport;
}

function renderDetail() {
  return render(
    <AuthProvider>
      <MemoryRouter
        initialEntries={["/migrations/job-1"]}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <Routes>
          <Route path="/migrations/:id" element={<MigrationDetail />} />
        </Routes>
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("MigrationDetail — what this migration does", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.getMigration).mockReset();
  });

  it("shows the qualified table name as the headline when no Name was given", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob());
    renderDetail();

    expect(await screen.findByRole("heading", { name: "public.orders" })).toBeInTheDocument();
  });

  it("shows the migration's Name as the headline, with the qualified table as a subtitle, when given", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Name: "Q3 billing update" }));
    renderDetail();

    expect(await screen.findByRole("heading", { name: "Q3 billing update" })).toBeInTheDocument();
    expect(screen.getByText("public.orders")).toBeInTheDocument();
  });

  it("shows the operation type badge and the human-readable summary", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob());
    renderDetail();

    expect(await screen.findByText("ADD_COLUMN")).toBeInTheDocument();
    expect(screen.getByText('Added column "status" (text) to public.orders')).toBeInTheDocument();
  });

  it("shows the reconstructed SQL statements", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob());
    renderDetail();

    expect(await screen.findByText(`ALTER TABLE "public"."orders" ADD COLUMN "status" text`)).toBeInTheDocument();
  });

  it("shows the operator-supplied description when present", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Description: "Adds the status column ahead of the promo launch" }),
    );
    renderDetail();

    expect(await screen.findByText("Adds the status column ahead of the promo launch")).toBeInTheDocument();
  });

  it("does not render a description block when none was given", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Description: "" }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    // Nothing meaningful to assert an ABSENCE of by text (empty string
    // renders nothing) — this test exists mainly to document the
    // intentional branch; the real coverage is the "present" case above.
  });

  it("does not render a SQL block for a SHADOW_TABLE type change (no deterministic statement)", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Operation: "ALTER_COLUMN_TYPE",
        Strategy: "SHADOW_TABLE",
        OperationSummary: "Changed column via a shadow table + logical replication",
        Statements: [],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Changed column via a shadow table + logical replication")).toBeInTheDocument();
    expect(screen.queryByText("SQL")).not.toBeInTheDocument();
  });
});

describe("MigrationDetail — affected records and step list", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.getMigration).mockReset();
  });

  it("shows RowsProcessed as the affected-records stat when it's the real, exact count", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ RowsProcessed: 1200, EstimatedRowCount: 5000 }),
    );
    renderDetail();

    expect(await screen.findByText("Rows processed")).toBeInTheDocument();
    expect(screen.getByText("1,200 / ~5,000")).toBeInTheDocument();
  });

  it("falls back to EstimatedRowCount, honestly labeled as an estimate, when nothing was individually processed", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ RowsProcessed: 0, EstimatedRowCount: 42000 }));
    renderDetail();

    expect(await screen.findByText("Affected records (est.)")).toBeInTheDocument();
    expect(screen.getByText("42,000")).toBeInTheDocument();
    expect(screen.queryByText("Rows processed")).not.toBeInTheDocument();
  });

  it("shows neither stat when there is no row-count information at all", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ RowsProcessed: 0, EstimatedRowCount: 0 }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Rows processed")).not.toBeInTheDocument();
    expect(screen.queryByText(/Affected records/)).not.toBeInTheDocument();
  });

  it("shows a textual step list below the phase track, with each stage's status in words", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        // CurrentPhase deliberately differs from any Stage's Phase name
        // below (the default "COMPLETED" would collide with the header's
        // status badge, which also renders CurrentPhase as text).
        CurrentPhase: "SYNCING",
        Stages: [
          { Phase: "PREPARATION", Status: "DONE" },
          { Phase: "SYNCING", Status: "CURRENT" },
          { Phase: "VALIDATING", Status: "PENDING" },
        ],
        Terminal: false,
      }),
    );
    renderDetail();

    expect(await screen.findByText("Steps")).toBeInTheDocument();
    // Scoped to the step list itself: PhaseTrack's own SVG also renders
    // each phase name as a <text> label (inside an aria-hidden group, but
    // still present in the DOM testing-library queries against), so an
    // unscoped getByText("PREPARATION") would be ambiguous between the
    // graphic and this new textual list.
    const stepList = screen.getByRole("list");
    expect(within(stepList).getByText("PREPARATION")).toBeInTheDocument();
    expect(within(stepList).getByText("Done")).toBeInTheDocument();
    expect(within(stepList).getByText("In progress")).toBeInTheDocument();
    expect(within(stepList).getByText("VALIDATING")).toBeInTheDocument();
    expect(within(stepList).getByText("Pending")).toBeInTheDocument();
  });

  it("shows the total duration for a terminal job next to the step list", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Terminal: true,
        CreatedAt: "2026-08-20T10:00:00Z",
        UpdatedAt: "2026-08-20T10:05:30Z",
      }),
    );
    renderDetail();

    await screen.findByText("Steps");
    // "5m 30s" legitimately appears twice — once next to "Steps" (the new
    // addition) and once as the existing Stats card's "Duration" —
    // both showing the same real duration is correct, not a bug, so this
    // asserts there are (at least) two matches rather than a single
    // unique one.
    expect(screen.getAllByText("5m 30.00s").length).toBeGreaterThanOrEqual(2);
  });

  // Direct regression coverage for a real usability request: each step
  // in the list needs its own icon (not just the color-coded Badge) —
  // a checkmark for Done, an X for Failed/Aborted, a spinner for In
  // progress — so the step's status reads at a glance, not just via the
  // badge text/color alone.
  it("shows a checkmark icon next to a Done step, distinct from the pending step's lack of one", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        CurrentPhase: "SYNCING",
        Stages: [
          { Phase: "PREPARATION", Status: "DONE" },
          { Phase: "SYNCING", Status: "CURRENT" },
          { Phase: "VALIDATING", Status: "PENDING" },
        ],
        Terminal: false,
      }),
    );
    renderDetail();

    const stepList = await screen.findByRole("list");
    const items = within(stepList).getAllByRole("listitem");
    // PREPARATION (Done): a checkmark path is present.
    expect(items[0].querySelector("svg path")).not.toBeNull();
    // VALIDATING (Pending): no icon at all — see stepStatusIcon's own
    // doc comment for why (an empty circle per pending step would just
    // be visual noise).
    expect(items[2].querySelector("svg")).toBeNull();
  });

  it("shows a spinning icon next to the genuinely in-progress step", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        CurrentPhase: "SYNCING",
        Stages: [
          { Phase: "PREPARATION", Status: "DONE" },
          { Phase: "SYNCING", Status: "CURRENT" },
        ],
        Terminal: false,
      }),
    );
    renderDetail();

    const stepList = await screen.findByRole("list");
    const items = within(stepList).getAllByRole("listitem");
    expect(items[1].querySelector("svg.motion-safe\\:animate-spin")).not.toBeNull();
  });

  // Direct regression test for the exact bug stepStatus itself already
  // guards against (see the describe block below) — the ICON must also
  // reflect "Failed", not "In progress", for a FAILED job's terminal
  // CURRENT stage.
  it("shows an X icon, not a spinner, for a step that failed", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        CurrentPhase: "FAILED",
        Failed: true,
        LastError: "duplicate key value violates unique constraint",
        Stages: [
          { Phase: "PREPARATION", Status: "DONE" },
          { Phase: "SYNCING", Status: "CURRENT" },
        ],
        Terminal: true,
      }),
    );
    renderDetail();

    const stepList = await screen.findByRole("list");
    const items = within(stepList).getAllByRole("listitem");
    expect(items[1].querySelector("svg.motion-safe\\:animate-spin")).toBeNull();
    expect(items[1].querySelector("svg path")).not.toBeNull();
  });
});

// This suite is a direct regression guard for a real, user-reported bug:
// a FAILED/ABORTED job's terminal stage is deliberately marked "CURRENT"
// server-side (see internal/progress.Compute's early-return path) purely
// so PhaseTrack's graphic highlights where things stopped — but the
// step list's naive "CURRENT" -> "In progress" mapping took that
// literally, showing "In progress" for a migration that had, in fact,
// stopped for good.
describe("stepStatus", () => {
  it("DONE always means Done, regardless of the job's outcome", () => {
    const result = stepStatus({ Phase: "PREPARATION", Status: "DONE" }, { Failed: true, CurrentPhase: "FAILED" });
    expect(result.label).toBe("Done");
    expect(result.tone).toBe("success");
  });

  it("PENDING always means Pending", () => {
    const result = stepStatus({ Phase: "COMPLETED", Status: "PENDING" }, { Failed: false, CurrentPhase: "SYNCING" });
    expect(result.label).toBe("Pending");
    expect(result.tone).toBe("neutral");
  });

  it("CURRENT on a genuinely still-running job means In progress", () => {
    const result = stepStatus({ Phase: "SYNCING", Status: "CURRENT" }, { Failed: false, CurrentPhase: "SYNCING" });
    expect(result.label).toBe("In progress");
    expect(result.tone).toBe("amber");
  });

  it("CURRENT on a FAILED job means Failed, not In progress — the exact bug this guards against", () => {
    const result = stepStatus({ Phase: "SYNCING", Status: "CURRENT" }, { Failed: true, CurrentPhase: "FAILED" });
    expect(result.label).toBe("Failed");
    expect(result.tone).toBe("coral");
  });

  it("CURRENT on an ABORTED job means Aborted, not In progress", () => {
    const result = stepStatus({ Phase: "SYNCING", Status: "CURRENT" }, { Failed: false, CurrentPhase: "ABORTED" });
    expect(result.label).toBe("Aborted");
    expect(result.tone).toBe("amber");
  });
});

describe("MigrationDetail — does not crash on a null Statements field", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.getMigration).mockReset();
  });

  it("does not show a replication lag indicator when ReplicationLagBytes is absent", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "DIRECT_DDL" }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Replication lag")).not.toBeInTheDocument();
  });

  it("shows the replication lag indicator with a growing trend for an in-progress SHADOW_TABLE job", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        CurrentPhase: "DELTA_SYNC",
        Terminal: false,
        ReplicationLagBytes: 47_185_920, // 45.0 MB
        ReplicationLagTrend: "growing",
      }),
    );
    renderDetail();

    expect(await screen.findByText("Replication lag")).toBeInTheDocument();
    expect(screen.getByText("45.0 MB")).toBeInTheDocument();
    expect(screen.getByText("Growing")).toBeInTheDocument();
  });

  it("shows a shrinking trend distinctly from a growing one", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        CurrentPhase: "DELTA_SYNC",
        Terminal: false,
        ReplicationLagBytes: 1024,
        ReplicationLagTrend: "shrinking",
      }),
    );
    renderDetail();

    expect(await screen.findByText("1.0 KB")).toBeInTheDocument();
    expect(screen.getByText("Shrinking — catching up")).toBeInTheDocument();
  });

  it("shows a measuring label when the trend is unknown (first reading)", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        CurrentPhase: "DELTA_SYNC",
        Terminal: false,
        ReplicationLagBytes: 0,
        ReplicationLagTrend: "unknown",
      }),
    );
    renderDetail();

    expect(await screen.findByText("Replication lag")).toBeInTheDocument();
    expect(screen.getByText("Measuring…")).toBeInTheDocument();
  });

  it("does not show a Resources section when ResourceStatus is absent", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "DIRECT_DDL" }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Resources")).not.toBeInTheDocument();
  });

  it("shows a green 'Cleaned up' badge for a resource confirmed gone", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        ResourceStatus: [
          { name: "Shadow table", detail: "__pgam_shadow_orders_job1", exists: false },
          { name: "Replication slot", detail: "pgam_slot_orders_job1", exists: false },
          { name: "Publication", detail: "pgam_pub_orders_job1", exists: false },
        ],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Resources")).toBeInTheDocument();
    expect(screen.getByText("Shadow table")).toBeInTheDocument();
    expect(screen.getByText("__pgam_shadow_orders_job1")).toBeInTheDocument();
    // All three are clean — three "Cleaned up" badges, no "Still present".
    expect(screen.getAllByText("Cleaned up")).toHaveLength(3);
    expect(screen.queryByText("Still present")).not.toBeInTheDocument();
  });

  // Direct regression test for the real incident this whole feature
  // exists to make visible: an orphaned shadow table sat completely
  // invisible after a failed migration until manually found via psql.
  // This confirms a lingering resource is shown with a clear, distinct
  // "Still present" warning — not silently indistinguishable from a
  // healthy, fully-cleaned-up migration.
  it("shows a coral 'Still present' badge for a resource that's still lingering", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Failed: true,
        LastError: "initial sync failed: duplicate key value violates unique constraint",
        ResourceStatus: [
          { name: "Shadow table", detail: "__pgam_shadow_orders_job1", exists: true },
          { name: "Replication slot", detail: "pgam_slot_orders_job1", exists: false },
          { name: "Publication", detail: "pgam_pub_orders_job1", exists: false },
        ],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Resources")).toBeInTheDocument();
    expect(screen.getByText("Still present")).toBeInTheDocument();
    // The other two are still reported clean, alongside the one problem.
    expect(screen.getAllByText("Cleaned up")).toHaveLength(2);
  });

  it("shows the leftover backfill index for an EXPAND_BACKFILL job", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "EXPAND_BACKFILL",
        ResourceStatus: [{ name: "Temporary backfill index", detail: "__pgam_backfill_idx_status_job1", exists: true }],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Temporary backfill index")).toBeInTheDocument();
    expect(screen.getByText("__pgam_backfill_idx_status_job1")).toBeInTheDocument();
    expect(screen.getByText("Still present")).toBeInTheDocument();
  });

  it("does not show a Resources section when ResourceStatus is an empty array", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "DIRECT_DDL", ResourceStatus: [] }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Resources")).not.toBeInTheDocument();
  });

  it("does not show a checkpoint pressure warning when CheckpointPressureDetected is absent", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: false }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText(/checkpoints are currently being forced/)).not.toBeInTheDocument();
  });

  // Direct regression test for a real incident this exists to surface:
  // PostgreSQL checkpoints forced every 6-22 seconds under heavy write
  // load, one taking 90 seconds, causing latency spikes that looked
  // identical to an application bug from the outside before anything in
  // this product could explain it.
  it("shows a checkpoint pressure warning when CheckpointPressureDetected is true", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Strategy: "SHADOW_TABLE", Terminal: false, CheckpointPressureDetected: true }),
    );
    renderDetail();

    expect(await screen.findByText(/checkpoints are currently being forced/)).toBeInTheDocument();
    expect(screen.getByText("max_wal_size")).toBeInTheDocument();
  });

  it("does not show the checkpoint pressure warning when CheckpointPressureDetected is explicitly false", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Strategy: "SHADOW_TABLE", Terminal: false, CheckpointPressureDetected: false }),
    );
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText(/checkpoints are currently being forced/)).not.toBeInTheDocument();
  });

  it("shows the impact-measurement opt-in checkbox for a running job, unchecked by default", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: false }));
    renderDetail();

    const checkbox = await screen.findByRole("checkbox", { name: /measure this migration's impact/i });
    expect(checkbox).not.toBeChecked();
  });

  it("does not show the impact-measurement checkbox for a terminal job — nothing live to measure", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: true }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByRole("checkbox", { name: /measure this migration's impact/i })).not.toBeInTheDocument();
  });

  it("does not show an impact reading before the checkbox is checked, even if the mock would return data", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Strategy: "SHADOW_TABLE", Terminal: false, ImpactActiveQueries: 3, ImpactPeakQueryDurationSeconds: 1.2 }),
    );
    renderDetail();

    await screen.findByRole("checkbox", { name: /measure this migration's impact/i });
    expect(screen.queryByText("Query impact on this table")).not.toBeInTheDocument();
  });

  // Direct regression test for the opt-in flow end to end: checking the
  // box must re-fetch WITH measureImpact=true (see api.getMigration's
  // own signature) and then display the reading once the (now
  // impact-including) response comes back.
  it("re-fetches with measureImpact=true and shows the reading once the checkbox is checked", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: false }));
    const user = userEvent.setup();
    renderDetail();

    const checkbox = await screen.findByRole("checkbox", { name: /measure this migration's impact/i });

    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Terminal: false,
        ImpactActiveQueries: 2,
        ImpactPeakQueryDurationSeconds: 0.85,
      }),
    );
    await user.click(checkbox);

    await waitFor(() => expect(api.getMigration).toHaveBeenCalledWith(expect.anything(), true));
    expect(await screen.findByText("Query impact on this table")).toBeInTheDocument();
    expect(screen.getByText(/2 active/)).toBeInTheDocument();
    expect(screen.getByText(/peak 0\.85s/)).toBeInTheDocument();
  });

  it("does not show the impact checkbox for a terminal job", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: true }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByRole("checkbox", { name: /measure this migration's impact/i })).not.toBeInTheDocument();
  });

  // Direct regression test for Faz D's "automatic post-migration impact
  // report" — the durable peak (see state.Job.ImpactPeakQueryDurationSeconds's
  // own doc comment) must show up automatically for a finished
  // migration, with no checkbox needed — unlike the live version, which
  // is opt-in specifically because of its ongoing per-poll query cost;
  // once the migration is done there's no ongoing cost left to gate.
  it("automatically shows the peak impact for a terminal job that had impact measurement turned on while running", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Strategy: "SHADOW_TABLE", Terminal: true, ImpactPeakQueryDurationSeconds: 2.35 }),
    );
    renderDetail();

    expect(await screen.findByText("Peak query impact during this migration")).toBeInTheDocument();
    expect(screen.getByText("2.35s")).toBeInTheDocument();
  });

  // Direct regression test for the "nil means never measured" contract
  // — a finished migration nobody ever checked the impact box for must
  // NOT show a misleading "0s", it must show nothing at all.
  it("does not show any impact report for a terminal job where impact measurement was never turned on", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({ Strategy: "SHADOW_TABLE", Terminal: true }));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Peak query impact during this migration")).not.toBeInTheDocument();
  });

  it("does not show the sustained-growth warning when ReplicationLagGrowingForSeconds is absent", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Terminal: false,
        ReplicationLagBytes: 50_000_000,
        ReplicationLagTrend: "growing",
      }),
    );
    renderDetail();

    await screen.findByText("Replication lag");
    expect(screen.queryByText(/may not converge/)).not.toBeInTheDocument();
  });

  // Direct regression test for the escalated signal this whole feature
  // exists to surface: a routine "Growing" badge alone (tested
  // elsewhere) is easy to miss during a long migration — several minutes
  // of UNBROKEN growth gets an explicit, hard-to-miss warning instead,
  // found necessary via a real load test where a SHADOW_TABLE
  // migration's delta sync genuinely never converged.
  it("shows an escalated 'may not converge' warning once lag has grown continuously for several minutes", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Terminal: false,
        ReplicationLagBytes: 500_000_000,
        ReplicationLagTrend: "growing",
        ReplicationLagGrowingForSeconds: 245, // 4m 5s
      }),
    );
    renderDetail();

    expect(await screen.findByText(/may not converge/)).toBeInTheDocument();
    expect(screen.getByText(/4m 5\.00s/)).toBeInTheDocument();
    // Deliberately advisory-only phrasing — never claims the tool acted
    // or will act automatically.
    expect(screen.getByText(/Consider stopping it/)).toBeInTheDocument();
  });

  it("does not show a Health summary for a simple terminal job with nothing beyond the outcome to report", async () => {
    // Default makeJob: Terminal=true, no VALIDATING stage, no
    // ResourceStatus — the common, simplest case (e.g. a quick
    // DIRECT_DDL) where the phase badge above already says everything
    // this card would.
    vi.mocked(api.getMigration).mockResolvedValue(makeJob({}));
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Health summary")).not.toBeInTheDocument();
  });

  it("does not show a Health summary for a non-terminal (still running) job", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Terminal: false,
        Stages: [{ Phase: "VALIDATING", Status: "PENDING" }],
        ResourceStatus: [{ name: "Shadow table", detail: "__pgam_shadow_orders_job1", exists: true }],
      }),
    );
    renderDetail();

    await screen.findByRole("heading", { name: "public.orders" });
    expect(screen.queryByText("Health summary")).not.toBeInTheDocument();
  });

  it("shows all-pass checks for a fully healthy completed SHADOW_TABLE migration", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Terminal: true,
        Failed: false,
        Stages: [{ Phase: "VALIDATING", Status: "DONE" }],
        ResourceStatus: [
          { name: "Shadow table", detail: "__pgam_shadow_orders_job1", exists: false },
          { name: "Replication slot", detail: "pgam_slot_orders_job1", exists: false },
        ],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Health summary")).toBeInTheDocument();
    expect(screen.getByText("Outcome")).toBeInTheDocument();
    expect(screen.getByText("Data validated")).toBeInTheDocument();
    expect(screen.getByText("Resources cleaned up")).toBeInTheDocument();
    expect(screen.getAllByText("✓")).toHaveLength(3);
    expect(screen.queryByText("⚠")).not.toBeInTheDocument();
  });

  // Direct regression test for the backend fix accompanying this: an
  // ADD_INDEX migration now gets its own explicit VALIDATING stage (see
  // internal/progress's pipelineFor and internal/ddlflow's
  // executeAddIndex), surfacing the index-validity check it already
  // enforced internally — this confirms the Health Card picks that up
  // automatically, with zero ADD_INDEX-specific frontend logic needed.
  it("shows 'Data validated' for a completed ADD_INDEX migration (DIRECT_DDL)", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "DIRECT_DDL",
        Operation: "ADD_INDEX",
        Terminal: true,
        Failed: false,
        Stages: [
          { Phase: "PREPARATION", Status: "DONE" },
          { Phase: "VALIDATING", Status: "DONE" },
          { Phase: "COMPLETED", Status: "DONE" },
        ],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Health summary")).toBeInTheDocument();
    expect(screen.getByText("Data validated")).toBeInTheDocument();
    expect(
      screen.queryByText("Row counts/checksums matched between source and shadow table"),
    ).not.toBeInTheDocument();
  });

  // Direct regression test for the exact real incident this whole
  // feature line exists to make visible at a glance: a migration that
  // failed AND left a resource behind — both must show as distinct
  // warnings, not just a single generic "something went wrong".
  it("shows warn checks for a failed migration with a lingering resource", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({
        Strategy: "SHADOW_TABLE",
        Terminal: true,
        Failed: true,
        LastError: "initial sync failed: duplicate key value violates unique constraint",
        Stages: [{ Phase: "VALIDATING", Status: "PENDING" }],
        ResourceStatus: [{ name: "Shadow table", detail: "__pgam_shadow_orders_job1", exists: true }],
      }),
    );
    renderDetail();

    expect(await screen.findByText("Health summary")).toBeInTheDocument();
    // Outcome fails, resources still present — two warnings. No
    // VALIDATING stage reached "DONE" either, so validation itself is
    // also flagged: three total.
    expect(screen.getAllByText("⚠")).toHaveLength(3);
    expect(screen.queryByText("✓")).not.toBeInTheDocument();
  });

  // Regression test for a real, user-reported crash: a Go nil slice
  // marshals to JSON `null`, and this screen used to do
  // `job.Statements.length` unconditionally — this exact bug was already
  // found and fixed once in internal/preview.Generate, then reintroduced
  // in internal/progress.describeOperation's SHADOW_TABLE/default
  // branches (both now fixed to return []string{} instead of nil). This
  // test guards the FRONTEND side: even if a backend regression ever
  // reintroduces a nil Statements value, the screen should degrade
  // gracefully, not crash the whole app.
  it("renders without crashing when Statements is null instead of an array", async () => {
    vi.mocked(api.getMigration).mockResolvedValue(
      makeJob({ Statements: null as unknown as string[] }),
    );
    renderDetail();

    expect(await screen.findByRole("heading", { name: "public.orders" })).toBeInTheDocument();
    expect(screen.queryByText("SQL")).not.toBeInTheDocument();
  });
});
