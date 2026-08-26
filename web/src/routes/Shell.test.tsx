import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
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
import { AuthProvider } from "../lib/auth";
import { Shell } from "./Shell";

function renderShellWithRoutes() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Shell>
          <Routes>
            <Route path="/" element={<h1>Migrations page</h1>} />
            <Route path="/users" element={<h1>Users page</h1>} />
            <Route path="/help" element={<h1>Help page</h1>} />
          </Routes>
        </Shell>
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("Shell accessibility", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "admin@b.com", role: "admin" });
  });

  it("has a skip link pointing at the main content landmark", async () => {
    renderShellWithRoutes();
    const skipLink = await screen.findByRole("link", { name: /skip to main content/i });
    expect(skipLink).toHaveAttribute("href", "#main-content");

    const main = screen.getByRole("main");
    expect(main).toHaveAttribute("id", "main-content");
  });

  it("gives the main landmark a nav label distinct from the page", async () => {
    renderShellWithRoutes();
    expect(await screen.findByRole("navigation", { name: /main navigation/i })).toBeInTheDocument();
  });

  // This is the single highest-value SPA accessibility fix: without it,
  // a screen reader or keyboard user gets no signal at all that the page
  // "changed" on client-side navigation — focus just silently stays
  // wherever it was, often on a link/button that no longer makes sense
  // (e.g. one from the OLD page's now-unmounted content).
  it("moves focus to the main landmark on every route change", async () => {
    renderShellWithRoutes();
    const main = await screen.findByRole("main");

    // tabIndex=-1 makes it programmatically focusable without joining
    // the normal Tab order — this is the correct WAI-ARIA pattern.
    expect(main).toHaveAttribute("tabindex", "-1");
    await waitFor(() => expect(document.activeElement).toBe(main));

    const user = userEvent.setup();
    // Move focus to a different real focusable element first (document.body
    // itself isn't focusable without a tabindex, so focusing it would be a
    // silent no-op) — this makes the assertion below actually prove
    // navigation moved focus, not that it simply never left main since mount.
    const signOutButton = screen.getByRole("button", { name: /sign out/i });
    signOutButton.focus();
    expect(document.activeElement).toBe(signOutButton);

    const usersLink = screen.getByRole("link", { name: "Users" });
    await user.click(usersLink);

    await waitFor(() => expect(screen.getByText("Users page")).toBeInTheDocument());
    await waitFor(() => expect(document.activeElement).toBe(main));
  });

  it("shows a Help link in the main navigation, reachable by every role (no minRole restriction)", async () => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "viewer@b.com", role: "viewer" });
    renderShellWithRoutes();

    const nav = await screen.findByRole("navigation", { name: /main navigation/i });
    const helpLink = within(nav).getByRole("link", { name: "Help" });
    expect(helpLink).toHaveAttribute("href", "/help");
  });

  it("shows a v1.0 label next to the pgArchiMigrator title in the header", async () => {
    renderShellWithRoutes();
    expect(await screen.findByText("pgArchiMigrator")).toBeInTheDocument();
    expect(screen.getByText("v1.0")).toBeInTheDocument();
  });
});
