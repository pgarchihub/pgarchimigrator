import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    startUpgrade: vi.fn(),
    introspectUpgradeSource: vi.fn(),
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

import { api, ApiError } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import NewUpgrade from "./NewUpgrade";

function renderScreen() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/upgrades/new"]}>
        <Routes>
          <Route path="/upgrades/new" element={<NewUpgrade />} />
          <Route path="/upgrades/:id" element={<div>Upgrade detail page</div>} />
        </Routes>
      </MemoryRouter>
    </AuthProvider>,
  );
}

// fillConnectionFields fills one ConnectionFieldsInput block (Source or
// Target) by its idPrefix, leaving port/SSL mode at their defaults —
// matches how a real user would only touch what they actually need to
// change.
async function fillConnectionFields(
  user: ReturnType<typeof userEvent.setup>,
  idPrefix: "source" | "target",
  host: string,
  username: string,
  password: string,
  database: string,
) {
  await user.type(screen.getByLabelText(/^host/i, { selector: `#${idPrefix}-host` }), host);
  await user.type(screen.getByLabelText(/^username/i, { selector: `#${idPrefix}-username` }), username);
  await user.type(screen.getByLabelText(/^password/i, { selector: `#${idPrefix}-password` }), password);
  await user.type(screen.getByLabelText(/^database/i, { selector: `#${idPrefix}-database` }), database);
}

describe("NewUpgrade", () => {
  // Without this, api.startUpgrade's own mock.calls array accumulates
  // across every test in this file (vitest doesn't clear mock call
  // history between tests by default) — any test reading
  // mock.calls[0] would silently pick up a PRIOR test's call instead
  // of its own, exactly the flaky failure this fixes.
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("submits assembled connection strings and navigates to the new job's detail page on success", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.startUpgrade).mockResolvedValue({
      id: "upgrade_new123",
      status: "accepted",
      statusUrl: "/api/upgrades/upgrade_new123",
    });
    const user = userEvent.setup();
    renderScreen();

    await screen.findByText(/start a database migration/i);
    await fillConnectionFields(user, "source", "old-host", "app_user", "secret1", "mydb");
    await fillConnectionFields(user, "target", "new-host", "app_user", "secret2", "mydb");
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    expect(await screen.findByText("Upgrade detail page")).toBeInTheDocument();
    expect(api.startUpgrade).toHaveBeenCalledWith(
      expect.objectContaining({
        sourceDsn: expect.stringContaining("old-host"),
        targetDsn: expect.stringContaining("new-host"),
      }),
    );
    const call = vi.mocked(api.startUpgrade).mock.calls[0][0];
    expect(call.sourceDsn).toContain("app_user:secret1@old-host");
    expect(call.targetDsn).toContain("app_user:secret2@new-host");
  });

  // Direct regression test for the whole point of Priority 2 — the
  // password field must be a real masked input, and the form must not
  // require typing a raw connection string at all.
  it("masks the password fields and has no raw connection-string input", () => {
    renderScreen();

    expect(screen.getByLabelText(/^password/i, { selector: "#source-password" })).toHaveAttribute("type", "password");
    expect(screen.getByLabelText(/^password/i, { selector: "#target-password" })).toHaveAttribute("type", "password");
    expect(screen.queryByPlaceholderText(/postgresql:\/\//)).not.toBeInTheDocument();
  });

  it("disables submit until source and target are both complete", async () => {
    const user = userEvent.setup();
    renderScreen();

    expect(screen.getByRole("button", { name: /start database migration/i })).toBeDisabled();

    await fillConnectionFields(user, "source", "old-host", "app_user", "secret1", "mydb");
    expect(screen.getByRole("button", { name: /start database migration/i })).toBeDisabled();

    await fillConnectionFields(user, "target", "new-host", "app_user", "secret2", "mydb");
    expect(screen.getByRole("button", { name: /start database migration/i })).not.toBeDisabled();
  });

  it("parses a comma-separated schemas field into an array when metadata was never fetched", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.startUpgrade).mockResolvedValue({ id: "j1", status: "accepted", statusUrl: "/api/upgrades/j1" });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    await user.type(screen.getByLabelText(/type schema names manually/i), "public, billing , reporting");
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    await screen.findByText("Upgrade detail page");
    expect(api.startUpgrade).toHaveBeenCalledWith(
      expect.objectContaining({ schemas: ["public", "billing", "reporting"] }),
    );
  });

  // Direct regression test for the "Advanced" replication override
  // reusing Source's own username/password/database — the whole reason
  // it only asks for host/port, see NewUpgrade.tsx's own comment.
  it("builds sourceReplicationDsn from the replication host plus Source's own credentials", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.startUpgrade).mockResolvedValue({ id: "j1", status: "accepted", statusUrl: "/api/upgrades/j1" });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "localhost", "app_user", "secret1", "mydb");
    await fillConnectionFields(user, "target", "new-host", "app_user", "secret2", "mydb");
    await user.click(screen.getByRole("button", { name: /advanced: different replication address/i }));
    await user.type(screen.getByLabelText(/replication host/i), "pg-logical");
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    await screen.findByText("Upgrade detail page");
    const call = vi.mocked(api.startUpgrade).mock.calls[0][0];
    expect(call.sourceReplicationDsn).toContain("app_user:secret1@pg-logical");
  });

  it("leaves sourceReplicationDsn undefined when the advanced override is never used", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.startUpgrade).mockResolvedValue({ id: "j1", status: "accepted", statusUrl: "/api/upgrades/j1" });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "localhost", "u", "p", "db");
    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    await screen.findByText("Upgrade detail page");
    const call = vi.mocked(api.startUpgrade).mock.calls[0][0];
    expect(call.sourceReplicationDsn).toBeUndefined();
  });

  it("shows an error message and does not navigate when the request fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.startUpgrade).mockRejectedValue(new ApiError(400, "sourceDsn and targetDsn are both required"));
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    expect(await screen.findByText("sourceDsn and targetDsn are both required")).toBeInTheDocument();
    expect(screen.queryByText("Upgrade detail page")).not.toBeInTheDocument();
  });

  // --- Priority 3: schema/table checkbox picker ---

  it("disables 'Fetch schemas & tables' until Source is complete", async () => {
    const user = userEvent.setup();
    renderScreen();

    expect(screen.getByRole("button", { name: /fetch schemas & tables/i })).toBeDisabled();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    expect(screen.getByRole("button", { name: /fetch schemas & tables/i })).not.toBeDisabled();
  });

  it("fetches and renders the schema/table picker, defaulting to everything selected", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockResolvedValue({
      schemas: [{ name: "public", tables: ["orders", "customers"] }],
    });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /fetch schemas & tables/i }));

    expect(await screen.findByText("orders")).toBeInTheDocument();
    expect(screen.getByText("customers")).toBeInTheDocument();
    expect(screen.getByText(/2 of 2 tables selected/i)).toBeInTheDocument();
    expect(api.introspectUpgradeSource).toHaveBeenCalledWith(expect.stringContaining("old-host"));
  });

  it("hides the manual schema text field once metadata has been fetched", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockResolvedValue({
      schemas: [{ name: "public", tables: ["orders"] }],
    });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    expect(screen.getByLabelText(/type schema names manually/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /fetch schemas & tables/i }));

    await screen.findByText("orders");
    expect(screen.queryByLabelText(/type schema names manually/i)).not.toBeInTheDocument();
  });

  it("shows an error message when introspection fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockRejectedValue(new ApiError(502, "could not connect to source"));
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "unreachable-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /fetch schemas & tables/i }));

    expect(await screen.findByText("could not connect to source")).toBeInTheDocument();
  });

  // Direct regression test for the whole point of Priority 3 — the
  // checkbox selection reaches the request as an explicit table list,
  // not just as accepted-and-ignored form state.
  it("submits the checkbox selection as an explicit tables list", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockResolvedValue({
      schemas: [{ name: "public", tables: ["orders", "customers"] }],
    });
    vi.mocked(api.startUpgrade).mockResolvedValue({ id: "j1", status: "accepted", statusUrl: "/api/upgrades/j1" });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /fetch schemas & tables/i }));
    await screen.findByText("orders");

    // Uncheck "customers" — only "orders" should remain selected.
    await user.click(screen.getByRole("checkbox", { name: "customers" }));
    await user.click(screen.getByRole("button", { name: /start database migration/i }));

    await screen.findByText("Upgrade detail page");
    const call = vi.mocked(api.startUpgrade).mock.calls[0][0];
    expect(call.tables).toEqual([{ schema: "public", table: "orders" }]);
    expect(call.schemas).toBeUndefined();
  });

  // --- Target-side read-only verification ---

  it("disables 'View existing schemas & tables' until Target is complete", async () => {
    const user = userEvent.setup();
    renderScreen();

    expect(screen.getByRole("button", { name: /view existing schemas & tables/i })).toBeDisabled();

    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    expect(screen.getByRole("button", { name: /view existing schemas & tables/i })).not.toBeDisabled();
  });

  it("shows the target's existing schemas and tables read-only, without affecting submission", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockResolvedValue({
      schemas: [{ name: "public", tables: ["legacy_orders"] }],
    });
    vi.mocked(api.startUpgrade).mockResolvedValue({ id: "j1", status: "accepted", statusUrl: "/api/upgrades/j1" });
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "source", "old-host", "u", "p", "db");
    await fillConnectionFields(user, "target", "new-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /view existing schemas & tables/i }));

    expect(await screen.findByText("legacy_orders")).toBeInTheDocument();
    expect(api.introspectUpgradeSource).toHaveBeenCalledWith(expect.stringContaining("new-host"));

    // Confirm this never leaks into the actual submitted request.
    await user.click(screen.getByRole("button", { name: /start database migration/i }));
    await screen.findByText("Upgrade detail page");
    const call = vi.mocked(api.startUpgrade).mock.calls[0][0];
    expect(call.tables).toBeUndefined();
    expect(call.schemas).toBeUndefined();
  });

  it("shows an error message when checking the target fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.introspectUpgradeSource).mockRejectedValue(new ApiError(502, "could not connect to target"));
    const user = userEvent.setup();
    renderScreen();

    await fillConnectionFields(user, "target", "unreachable-host", "u", "p", "db");
    await user.click(screen.getByRole("button", { name: /view existing schemas & tables/i }));

    expect(await screen.findByText("could not connect to target")).toBeInTheDocument();
  });
});
