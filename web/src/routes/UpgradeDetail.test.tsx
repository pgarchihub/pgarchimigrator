import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    getUpgrade: vi.fn(),
    retryUpgrade: vi.fn(),
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
import type { UpgradeDetail as UpgradeDetailType } from "../lib/types";
import { AuthProvider } from "../lib/auth";
import UpgradeDetail from "./UpgradeDetail";

function makeDetail(overrides: Partial<UpgradeDetailType> = {}): UpgradeDetailType {
  return {
    ID: "upgrade_abc123",
    Phase: "SYNCING",
    Schemas: ["public"],
    SourceConnectionRef: "postgresql://redacted",
    TargetConnectionRef: "postgresql://redacted",
    SourceReplicationRef: "",
    Tables: null,
    LastError: "",
    CreatedAt: "2026-09-04T10:00:00Z",
    UpdatedAt: "2026-09-04T10:05:00Z",
    TablesTotal: 2,
    TablesSynced: 1,
    TablesVerified: 0,
    tables: [
      {
        JobID: "upgrade_abc123",
        SchemaName: "public",
        TableName: "orders",
        Phase: "SYNCING",
        RowsSynced: 500,
        LastError: "",
      },
      {
        JobID: "upgrade_abc123",
        SchemaName: "public",
        TableName: "customers",
        Phase: "SCHEMA_CREATED",
        RowsSynced: 0,
        LastError: "",
      },
    ],
    ...overrides,
  };
}

function renderScreen() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/upgrades/upgrade_abc123"]}>
        <Routes>
          <Route path="/upgrades/:id" element={<UpgradeDetail />} />
        </Routes>
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("UpgradeDetail", () => {
  it("shows the job id, phase, and table-level progress", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail());
    renderScreen();

    expect(await screen.findByText("upgrade_abc123")).toBeInTheDocument();
    expect(screen.getAllByText("SYNCING").length).toBeGreaterThan(0); // appears both on the job's own badge and the "orders" table row
    expect(screen.getByText("public.orders")).toBeInTheDocument();
    expect(screen.getByText("public.customers")).toBeInTheDocument();
    expect(screen.getByText("500")).toBeInTheDocument();
  });

  it("shows the failure message when the job failed", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    renderScreen();

    expect(await screen.findByText(/connection refused/)).toBeInTheDocument();
  });

  it("shows a success banner and no phase-step indicator once ready", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "READY", TablesVerified: 2, TablesSynced: 2 }));
    renderScreen();

    expect(await screen.findByText(/does not perform cutover/)).toBeInTheDocument();
    expect(screen.queryByLabelText(/upgrade progress/i)).not.toBeInTheDocument();
  });

  it("shows a placeholder message while still introspecting and no tables exist yet", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "INTROSPECTING", tables: null }));
    renderScreen();

    expect(await screen.findByText(/discovering tables on the source instance/i)).toBeInTheDocument();
  });

  it("shows an error message when the job can't be loaded", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    const { ApiError } = await import("../lib/api");
    vi.mocked(api.getUpgrade).mockRejectedValue(new ApiError(404, "upgrade job upgrade_abc123 not found"));
    renderScreen();

    expect(await screen.findByText(/not found/)).toBeInTheDocument();
  });
});

describe("UpgradeDetail — retry", () => {
  // Without this, api.retryUpgrade's own mock.calls array accumulates
  // across every test in this describe block (vitest doesn't clear
  // mock call history between tests by default) — a test reading
  // "was retryUpgrade called" would otherwise silently see a PRIOR
  // test's call too.
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows a Retry button only for a failed upgrade", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    renderScreen();

    expect(await screen.findByRole("button", { name: /retry this database migration/i })).toBeInTheDocument();
  });

  it("hides the Retry button for an upgrade that did not fail", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "READY" }));
    renderScreen();

    await screen.findByText("upgrade_abc123");
    expect(screen.queryByRole("button", { name: /retry this database migration/i })).not.toBeInTheDocument();
  });

  it("navigates to the new job's detail page after a successful retry", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockImplementation(async (id: string) =>
      id === "upgrade_new456"
        ? makeDetail({ ID: "upgrade_new456", Phase: "INTROSPECTING" })
        : makeDetail({ Phase: "FAILED", LastError: "connection refused" }),
    );
    vi.mocked(api.retryUpgrade).mockResolvedValue({
      id: "upgrade_new456",
      status: "accepted",
      statusUrl: "/api/upgrades/upgrade_new456",
    });
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.click(await screen.findByRole("button", { name: /confirm retry/i }));

    expect(api.retryUpgrade).toHaveBeenCalledWith("upgrade_abc123", undefined);
    await waitFor(() => expect(api.getUpgrade).toHaveBeenCalledWith("upgrade_new456"));
  });

  it("shows an error message when the retry request fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    const { ApiError } = await import("../lib/api");
    vi.mocked(api.retryUpgrade).mockRejectedValue(new ApiError(500, "retry failed unexpectedly"));
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.click(await screen.findByRole("button", { name: /confirm retry/i }));

    expect(await screen.findByText(/retry failed unexpectedly/)).toBeInTheDocument();
  });

  // --- Retry review panel ---

  // Direct regression test for the actual ask: clicking "Retry" must
  // NOT immediately call the API — it must show what will be reused
  // first, since a retry silently reusing wrong stored values (this
  // exact scenario: a job whose source connection had no working
  // replication override) previously gave no way to notice before it
  // failed the same way again.
  it("does not call the API until the review panel is confirmed", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));

    expect(api.retryUpgrade).not.toHaveBeenCalled();
    expect(screen.getByText(/review before retrying/i)).toBeInTheDocument();
  });

  it("shows the source and target host/username/database being reused, never the password", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(
      makeDetail({
        Phase: "FAILED",
        LastError: "connection refused",
        SourceConnectionRef: "postgresql://app_user:supersecret@old-host:5432/mydb",
        TargetConnectionRef: "postgresql://app_user:othersecret@new-host:5432/mydb",
      }),
    );
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));

    expect(screen.getByText(/app_user@old-host:5432 \/ mydb/)).toBeInTheDocument();
    expect(screen.getByText(/app_user@new-host:5432 \/ mydb/)).toBeInTheDocument();
    // The critical guarantee — neither password ever reaches the DOM.
    expect(document.body.textContent).not.toContain("supersecret");
    expect(document.body.textContent).not.toContain("othersecret");
  });

  it("warns when no replication override is on file", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(
      makeDetail({ Phase: "FAILED", LastError: "connection refused", SourceReplicationRef: "" }),
    );
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));

    expect(screen.getByText(/no replication address override on file/i)).toBeInTheDocument();
  });

  it("shows the replication override when one is on file", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(
      makeDetail({
        Phase: "FAILED",
        LastError: "connection refused",
        SourceReplicationRef: "postgresql://app_user:secret@pg-logical:5432/mydb",
      }),
    );
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));

    expect(screen.getByText(/source replication address/i)).toBeInTheDocument();
    expect(screen.getByText(/app_user@pg-logical:5432 \/ mydb/)).toBeInTheDocument();
  });

  it("shows the selected tables when the job used table-level scoping", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(
      makeDetail({
        Phase: "FAILED",
        LastError: "connection refused",
        Schemas: null,
        Tables: [
          { schema: "public", table: "orders" },
          { schema: "reporting", table: "events" },
        ],
      }),
    );
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));

    expect(screen.getByText(/2 specific table\(s\)/i)).toBeInTheDocument();
    expect(screen.getByText(/public\.orders, reporting\.events/i)).toBeInTheDocument();
  });

  it("cancels back to the plain Retry button without calling the API", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.click(screen.getByRole("button", { name: /cancel/i }));

    expect(api.retryUpgrade).not.toHaveBeenCalled();
    expect(screen.queryByText(/review before retrying/i)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry this database migration/i })).toBeInTheDocument();
  });

  // Direct regression test for the actual, repeated bug report: a
  // retry whose original job never had a working replication override
  // must be FIXABLE right from the review panel, not just describable
  // as broken — this is what makes the panel more than a read-only
  // warning.
  it("lets the user set a replication host/port override before confirming", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(
      makeDetail({ Phase: "FAILED", LastError: "connection refused", SourceReplicationRef: "" }),
    );
    vi.mocked(api.retryUpgrade).mockResolvedValue({ id: "j2", status: "accepted", statusUrl: "/api/upgrades/j2" });
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.type(screen.getByLabelText(/^replication host/i), "pg-logical");
    await user.click(screen.getByRole("button", { name: /confirm retry/i }));

    expect(api.retryUpgrade).toHaveBeenCalledWith("upgrade_abc123", {
      replicationHost: "pg-logical",
      replicationPort: undefined,
    });
  });

  it("passes both host and port when both are filled in", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    vi.mocked(api.retryUpgrade).mockResolvedValue({ id: "j2", status: "accepted", statusUrl: "/api/upgrades/j2" });
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.type(screen.getByLabelText(/^replication host/i), "pg-logical");
    await user.type(screen.getByLabelText(/replication port/i), "5433");
    await user.click(screen.getByRole("button", { name: /confirm retry/i }));

    expect(api.retryUpgrade).toHaveBeenCalledWith("upgrade_abc123", {
      replicationHost: "pg-logical",
      replicationPort: "5433",
    });
  });

  it("sends no override when the replication fields are left blank", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.getUpgrade).mockResolvedValue(makeDetail({ Phase: "FAILED", LastError: "connection refused" }));
    vi.mocked(api.retryUpgrade).mockResolvedValue({ id: "j2", status: "accepted", statusUrl: "/api/upgrades/j2" });
    const user = userEvent.setup();
    renderScreen();

    await user.click(await screen.findByRole("button", { name: /retry this database migration/i }));
    await user.click(screen.getByRole("button", { name: /confirm retry/i }));

    expect(api.retryUpgrade).toHaveBeenCalledWith("upgrade_abc123", undefined);
  });
});
