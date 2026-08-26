import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    listMigrations: vi.fn(),
    getAnalytics: vi.fn().mockResolvedValue({
      totalMigrations: 0, terminalMigrations: 0, failureRate: 0, averageDurationMs: 0, strategyBreakdown: {},
    }),
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
import Dashboard, { jobMatchesDateRange, jobMatchesStatusFilter } from "./Dashboard";

function baseJob(overrides: Record<string, unknown> = {}) {
  return makeJob(overrides);
}
function makeJob(overrides: Record<string, unknown>): MigrationReport {
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
    OperationSummary: "Added column",
    Statements: [],
    ...overrides,
  } as unknown as MigrationReport;
}

function renderDashboard() {
  return render(
    <AuthProvider>
      <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Dashboard />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("Dashboard", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listMigrations).mockReset();
  });

  it("shows the qualified table name as the row link when no Name was given", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob()]);
    renderDashboard();

    expect(await screen.findByRole("link", { name: "public.orders" })).toBeInTheDocument();
  });

  it("shows the migration's Name as the row link, with the qualified table as a secondary line, when a Name was given", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Name: "Q3 billing update" })]);
    renderDashboard();

    expect(await screen.findByRole("link", { name: "Q3 billing update" })).toBeInTheDocument();
    expect(screen.getByText("public.orders")).toBeInTheDocument();
  });

  // The core ask this test guards: every row needs its own details link
  // to the migration detail page, not just the (easy-to-miss) name link.
  it("gives every row a details link pointing at its own migration detail page", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ JobID: "job-1", TableName: "orders" }),
      baseJob({ JobID: "job-2", TableName: "invoices", SchemaName: "billing" }),
    ]);
    renderDashboard();

    const detailsLink1 = await screen.findByRole("link", { name: /view details for public\.orders/i });
    const detailsLink2 = await screen.findByRole("link", { name: /view details for billing\.invoices/i });
    expect(detailsLink1).toHaveAttribute("href", "/migrations/job-1");
    expect(detailsLink2).toHaveAttribute("href", "/migrations/job-2");
    // Distinct accessible names, not just distinct hrefs — a screen
    // reader user tabbing through icon-only links needs to be able to
    // tell them apart by ear.
    expect(detailsLink1.getAttribute("aria-label")).not.toBe(detailsLink2.getAttribute("aria-label"));
  });

  it("uses the migration's Name in the details link's accessible name when one was given", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Name: "Q3 billing update" })]);
    renderDashboard();

    await waitFor(() =>
      expect(screen.getByRole("link", { name: /view details for q3 billing update/i })).toBeInTheDocument(),
    );
  });

  it("shows the Duration column using the same h/m/s format as the Migration Detail page", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ Terminal: true, CreatedAt: "2026-08-20T10:00:00Z", UpdatedAt: "2026-08-20T10:00:05Z" }),
    ]);
    renderDashboard();

    expect(await screen.findByText("5.00s")).toBeInTheDocument();
  });

  // Direct regression test for the real gap this column fixes: a
  // migration finishing in well under a second (a typical DIRECT_DDL)
  // used to show nothing distinguishing it from one that hadn't started
  // — this confirms the sub-second value is genuinely visible, not
  // collapsed to "0".
  it("shows millisecond-level precision for a sub-second migration, not a bare '0s'", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ Terminal: true, CreatedAt: "2026-08-20T10:00:00.000Z", UpdatedAt: "2026-08-20T10:00:00.050Z" }),
    ]);
    renderDashboard();

    expect(await screen.findByText("0.05s")).toBeInTheDocument();
  });

  it("does not show the Fleet summary card while analytics is still loading or has no terminal migrations", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Terminal: false })]);
    vi.mocked(api.getAnalytics).mockResolvedValue({
      totalMigrations: 1, terminalMigrations: 0, failureRate: 0, averageDurationMs: 0, strategyBreakdown: {},
    });
    renderDashboard();

    await screen.findByRole("table");
    expect(screen.queryByText("Fleet summary")).not.toBeInTheDocument();
  });

  it("shows the Fleet summary card with overall stats once analytics has terminal migrations", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Terminal: true })]);
    vi.mocked(api.getAnalytics).mockResolvedValue({
      totalMigrations: 4,
      terminalMigrations: 4,
      failureRate: 0.25,
      averageDurationMs: 90_000,
      strategyBreakdown: {
        DIRECT_DDL: { count: 3, failureRate: 0, averageDurationMs: 500 },
        SHADOW_TABLE: { count: 1, failureRate: 1, averageDurationMs: 2_700_000 },
      },
    });
    renderDashboard();

    const summaryCard = (await screen.findByText("Fleet summary")).closest("div")!.parentElement!;
    expect(within(summaryCard).getByText("25.0%")).toBeInTheDocument();
    expect(within(summaryCard).getByText("1m 30.00s")).toBeInTheDocument();
    expect(within(summaryCard).getByText("DIRECT_DDL")).toBeInTheDocument();
    expect(within(summaryCard).getByText("SHADOW_TABLE")).toBeInTheDocument();
  });

  // Direct regression test for the real reasoning behind the
  // per-strategy breakdown existing at all: a fleet dominated by many
  // fast DIRECT_DDL jobs shouldn't make a strategy with its own genuine
  // problem invisible in a single overall number.
  it("sorts the per-strategy breakdown by count, most-used first", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Terminal: true })]);
    vi.mocked(api.getAnalytics).mockResolvedValue({
      totalMigrations: 5,
      terminalMigrations: 5,
      failureRate: 0,
      averageDurationMs: 1000,
      strategyBreakdown: {
        SHADOW_TABLE: { count: 1, failureRate: 0, averageDurationMs: 1000 },
        DIRECT_DDL: { count: 4, failureRate: 0, averageDurationMs: 1000 },
      },
    });
    renderDashboard();

    await screen.findByText("Fleet summary");
    const rows = screen.getAllByText(/DIRECT_DDL|SHADOW_TABLE/);
    expect(rows[0]).toHaveTextContent("DIRECT_DDL");
    expect(rows[1]).toHaveTextContent("SHADOW_TABLE");
  });
});

describe("jobMatchesStatusFilter", () => {
  it("'' (All) matches everything", () => {
    expect(jobMatchesStatusFilter(baseJob(), "")).toBe(true);
    expect(jobMatchesStatusFilter(baseJob({ Failed: true }), "")).toBe(true);
  });

  it("IN_PROGRESS matches a non-terminal job that isn't in its rollback window", () => {
    expect(jobMatchesStatusFilter(baseJob({ Terminal: false, CurrentPhase: "SYNCING" }), "IN_PROGRESS")).toBe(true);
    expect(jobMatchesStatusFilter(baseJob({ Terminal: true }), "IN_PROGRESS")).toBe(false);
    expect(
      jobMatchesStatusFilter(baseJob({ Terminal: false, CurrentPhase: "ROLLBACK_WINDOW" }), "IN_PROGRESS"),
    ).toBe(false);
  });

  it("ROLLBACK_WINDOW matches only jobs currently in that phase", () => {
    expect(jobMatchesStatusFilter(baseJob({ CurrentPhase: "ROLLBACK_WINDOW" }), "ROLLBACK_WINDOW")).toBe(true);
    expect(jobMatchesStatusFilter(baseJob({ CurrentPhase: "SYNCING" }), "ROLLBACK_WINDOW")).toBe(false);
  });

  it("COMPLETED requires Terminal, not Failed, and CurrentPhase===COMPLETED all together", () => {
    expect(
      jobMatchesStatusFilter(baseJob({ Terminal: true, Failed: false, CurrentPhase: "COMPLETED" }), "COMPLETED"),
    ).toBe(true);
    // Terminal but FAILED must not count as completed.
    expect(
      jobMatchesStatusFilter(baseJob({ Terminal: true, Failed: true, CurrentPhase: "COMPLETED" }), "COMPLETED"),
    ).toBe(false);
    // Terminal and not failed, but ABORTED rather than COMPLETED.
    expect(
      jobMatchesStatusFilter(baseJob({ Terminal: true, Failed: false, CurrentPhase: "ABORTED" }), "COMPLETED"),
    ).toBe(false);
  });

  it("FAILED matches purely on the Failed flag", () => {
    expect(jobMatchesStatusFilter(baseJob({ Failed: true }), "FAILED")).toBe(true);
    expect(jobMatchesStatusFilter(baseJob({ Failed: false }), "FAILED")).toBe(false);
  });

  it("ABORTED matches purely on CurrentPhase", () => {
    expect(jobMatchesStatusFilter(baseJob({ CurrentPhase: "ABORTED" }), "ABORTED")).toBe(true);
    expect(jobMatchesStatusFilter(baseJob({ CurrentPhase: "COMPLETED" }), "ABORTED")).toBe(false);
  });
});

describe("jobMatchesDateRange", () => {
  it("matches everything when both bounds are empty", () => {
    expect(jobMatchesDateRange(baseJob({ CreatedAt: "2026-08-20T10:00:00Z" }), "", "")).toBe(true);
  });

  it("excludes a job created before the 'from' bound", () => {
    const job = baseJob({ CreatedAt: "2026-08-10T10:00:00Z" });
    expect(jobMatchesDateRange(job, "2026-08-15", "")).toBe(false);
    expect(jobMatchesDateRange(job, "2026-08-05", "")).toBe(true);
  });

  it("excludes a job created after the 'to' bound", () => {
    const job = baseJob({ CreatedAt: "2026-08-20T10:00:00Z" });
    expect(jobMatchesDateRange(job, "", "2026-08-15")).toBe(false);
    expect(jobMatchesDateRange(job, "", "2026-08-25")).toBe(true);
  });

  // The most easily-gotten-wrong case: a job created ON the 'to' date
  // itself (just not at midnight) must still match — the 'to' bound has
  // to mean "through the end of this day", not "before midnight of this
  // day", or picking today's date as the upper bound would exclude
  // everything created today.
  it("treats the 'to' bound as inclusive of the entire day", () => {
    const job = baseJob({ CreatedAt: "2026-08-20T23:30:00Z" });
    expect(jobMatchesDateRange(job, "", "2026-08-20")).toBe(true);
  });

  it("respects both bounds together as a range", () => {
    const job = baseJob({ CreatedAt: "2026-08-20T10:00:00Z" });
    expect(jobMatchesDateRange(job, "2026-08-15", "2026-08-25")).toBe(true);
    expect(jobMatchesDateRange(job, "2026-08-21", "2026-08-25")).toBe(false);
    expect(jobMatchesDateRange(job, "2026-08-01", "2026-08-19")).toBe(false);
  });
});

describe("Dashboard filter UI", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listMigrations).mockReset();
  });

  it("does not show the filter bar when there are no migrations at all", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([]);
    renderDashboard();

    await screen.findByText("No migrations yet");
    expect(screen.queryByText("Strategy")).not.toBeInTheDocument();
  });

  it("filters the visible rows by Strategy", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ JobID: "j1", TableName: "orders", Strategy: "DIRECT_DDL" }),
      baseJob({ JobID: "j2", TableName: "invoices", Strategy: "SHADOW_TABLE" }),
    ]);
    const user = userEvent.setup();
    renderDashboard();

    await screen.findByRole("link", { name: "public.orders" });
    expect(screen.getByRole("link", { name: "public.invoices" })).toBeInTheDocument();

    const strategySelect = screen.getByLabelText("Strategy");
    await user.selectOptions(strategySelect, "SHADOW_TABLE");

    expect(screen.queryByRole("link", { name: "public.orders" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "public.invoices" })).toBeInTheDocument();
  });

  it("filters the visible rows by Status", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ JobID: "j1", TableName: "orders", Failed: true, Terminal: true }),
      baseJob({ JobID: "j2", TableName: "invoices", Failed: false, Terminal: true, CurrentPhase: "COMPLETED" }),
    ]);
    const user = userEvent.setup();
    renderDashboard();

    await screen.findByRole("link", { name: "public.orders" });

    const statusSelect = screen.getByLabelText("Status");
    await user.selectOptions(statusSelect, "FAILED");

    expect(screen.getByRole("link", { name: "public.orders" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "public.invoices" })).not.toBeInTheDocument();
  });

  it("shows a 'no migrations match' empty state (distinct from the zero-migrations state) when filters exclude everything", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([baseJob({ Strategy: "DIRECT_DDL" })]);
    const user = userEvent.setup();
    renderDashboard();

    await screen.findByRole("link", { name: "public.orders" });
    const strategySelect = screen.getByLabelText("Strategy");
    await user.selectOptions(strategySelect, "SHADOW_TABLE");

    expect(await screen.findByText("No migrations match these filters")).toBeInTheDocument();
    expect(screen.queryByText("No migrations yet")).not.toBeInTheDocument();
  });

  it("Clear filters resets every filter and restores the full list", async () => {
    vi.mocked(api.listMigrations).mockResolvedValue([
      baseJob({ JobID: "j1", TableName: "orders", Strategy: "DIRECT_DDL" }),
      baseJob({ JobID: "j2", TableName: "invoices", Strategy: "SHADOW_TABLE" }),
    ]);
    const user = userEvent.setup();
    renderDashboard();

    await screen.findByRole("link", { name: "public.orders" });
    await user.selectOptions(screen.getByLabelText("Strategy"), "SHADOW_TABLE");
    expect(screen.queryByRole("link", { name: "public.orders" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /clear filters/i }));

    expect(await screen.findByRole("link", { name: "public.orders" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "public.invoices" })).toBeInTheDocument();
    expect((screen.getByLabelText("Strategy") as HTMLSelectElement).value).toBe("");
  });
});
