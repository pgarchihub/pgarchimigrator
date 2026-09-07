import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    listUpgrades: vi.fn(),
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
import type { UpgradeJob } from "../lib/types";
import { AuthProvider } from "../lib/auth";
import UpgradesList from "./UpgradesList";

function makeUpgrade(overrides: Partial<UpgradeJob> = {}): UpgradeJob {
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
    TablesTotal: 5,
    TablesSynced: 2,
    TablesVerified: 1,
    ...overrides,
  };
}

function renderScreen() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/upgrades"]}>
        <UpgradesList />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("UpgradesList", () => {
  it("shows a loading state before jobs arrive", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.listUpgrades).mockReturnValue(new Promise(() => {})); // never resolves
    renderScreen();

    expect(await screen.findByText(/loading upgrades/i)).toBeInTheDocument();
  });

  it("shows an empty state with no upgrades", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.listUpgrades).mockResolvedValue([]);
    renderScreen();

    expect(await screen.findByText(/no upgrades yet/i)).toBeInTheDocument();
  });

  it("lists upgrade jobs with their phase and table counts", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.listUpgrades).mockResolvedValue([makeUpgrade()]);
    renderScreen();

    expect(await screen.findByText("upgrade_abc123")).toBeInTheDocument();
    expect(screen.getByText("SYNCING")).toBeInTheDocument();
    expect(screen.getByText("1 / 5")).toBeInTheDocument();
  });

  // Direct regression test for this screen NEVER rendering the raw
  // connection strings — see UpgradesList.tsx's own file-level security
  // note (SourceConnectionRef/TargetConnectionRef can carry a password
  // and must never reach the UI).
  it("never renders the raw source/target connection strings", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.listUpgrades).mockResolvedValue([
      makeUpgrade({ SourceConnectionRef: "postgresql://user:supersecret@host/db" }),
    ]);
    renderScreen();

    await screen.findByText("upgrade_abc123");
    expect(screen.queryByText(/supersecret/)).not.toBeInTheDocument();
  });

  it("shows 'Start database migration' for an admin", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    vi.mocked(api.listUpgrades).mockResolvedValue([]);
    renderScreen();

    expect(await screen.findAllByRole("link", { name: /start database migration/i })).not.toHaveLength(0);
  });

  it("hides 'Start database migration' for a viewer", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "viewer@test.local", role: "viewer" });
    vi.mocked(api.listUpgrades).mockResolvedValue([]);
    renderScreen();

    await screen.findByText(/no upgrades yet/i);
    expect(screen.queryByRole("link", { name: /start database migration/i })).not.toBeInTheDocument();
  });

  it("shows an error message when the list request fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@test.local", role: "admin" });
    const { ApiError } = await import("../lib/api");
    vi.mocked(api.listUpgrades).mockRejectedValue(
      new ApiError(503, "database upgrade is not configured on this instance"),
    );
    renderScreen();

    expect(await screen.findByText(/not configured on this instance/i)).toBeInTheDocument();
  });
});
