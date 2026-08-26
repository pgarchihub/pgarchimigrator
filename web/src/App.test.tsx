import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("./lib/api", () => ({
  api: {
    me: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    setupRequired: vi.fn(),
    setup: vi.fn(),
    getVersion: vi.fn().mockResolvedValue({ version: "test" }),
    listMigrations: vi.fn().mockResolvedValue([]),
    getAnalytics: vi.fn().mockResolvedValue({
      totalMigrations: 0, terminalMigrations: 0, failureRate: 0, averageDurationMs: 0, strategyBreakdown: {},
    }),
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

import { api, ApiError } from "./lib/api";
import App from "./App";

function renderApp(initialPath: string) {
  return render(
    <MemoryRouter initialEntries={[initialPath]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      <App />
    </MemoryRouter>,
  );
}

// These tests cover App.tsx's routing decision itself — the actual
// screens (Login, Setup, Dashboard) have their own dedicated test files;
// what matters here is which ONE of them ends up on screen for a given
// combination of setupRequired + auth status + requested path.
describe("App routing — setup wizard vs. login vs. the app", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset();
    vi.mocked(api.setupRequired).mockReset();
  });

  it("routes to the setup wizard when no admin exists yet, regardless of the requested path", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.setupRequired).mockResolvedValue({ required: true });
    renderApp("/");

    expect(await screen.findByText(/set up your admin account/i)).toBeInTheDocument();
  });

  it("redirects /login to the setup wizard too, when setup is required", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.setupRequired).mockResolvedValue({ required: true });
    renderApp("/login");

    expect(await screen.findByText(/set up your admin account/i)).toBeInTheDocument();
  });

  it("shows the ordinary login screen once setup is no longer required", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.setupRequired).mockResolvedValue({ required: false });
    renderApp("/");

    expect(await screen.findByText(/sign in to migrator/i)).toBeInTheDocument();
  });

  it("redirects /setup to /login once a deployment is already bootstrapped (one-time wizard)", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.setupRequired).mockResolvedValue({ required: false });
    renderApp("/setup");

    expect(await screen.findByText(/sign in to migrator/i)).toBeInTheDocument();
  });

  it("shows the app itself when authenticated and setup is not required", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "admin@b.com", role: "admin" });
    vi.mocked(api.setupRequired).mockResolvedValue({ required: false });
    renderApp("/");

    expect(await screen.findByRole("heading", { name: "Migrations" })).toBeInTheDocument();
  });
});
