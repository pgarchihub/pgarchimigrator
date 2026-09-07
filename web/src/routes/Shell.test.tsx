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
      <MemoryRouter initialEntries={["/"]}>
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
    // "Sign out" is now inside the collapsible user menu (see Shell.tsx),
    // so it must be opened before the button is reachable.
    await user.click(screen.getByRole("button", { name: /admin/i }));
    const signOutButton = await screen.findByRole("menuitem", { name: /sign out/i });
    signOutButton.focus();
    expect(document.activeElement).toBe(signOutButton);

    const usersLink = screen.getByRole("link", { name: "Users" });
    await user.click(usersLink);

    await waitFor(() => expect(screen.getByText("Users page")).toBeInTheDocument());
    await waitFor(() => expect(document.activeElement).toBe(main));
  });

  // Direct regression test for Help's own deliberate placement: it
  // lives in the header (an always-available reference, regardless of
  // which tool you're using), not the sidebar's nav list — see
  // Shell.tsx's own comment on that split.
  it("shows a Help link in the header, reachable by every role (no minRole restriction)", async () => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "viewer@b.com", role: "viewer" });
    renderShellWithRoutes();

    const banner = await screen.findByRole("banner");
    const helpLink = within(banner).getByRole("link", { name: "Help" });
    expect(helpLink).toHaveAttribute("href", "/help");
  });

  it("shows a v2.0 label next to the pgArchiMigrator title in the header", async () => {
    renderShellWithRoutes();
    expect(await screen.findByText("pgArchiMigrator")).toBeInTheDocument();
    expect(screen.getByText("v2.0")).toBeInTheDocument();
  });

  // Direct regression test for the actual bug report: the logo wasn't
  // clickable at all before this fix.
  it("makes the logo a link back to the home page", async () => {
    renderShellWithRoutes();
    const logoLink = await screen.findByRole("link", { name: /pgArchiMigrator/i });
    expect(logoLink).toHaveAttribute("href", "/");
  });
});

describe("Shell — collapsible sidebar", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "admin@b.com", role: "admin" });
    window.localStorage.clear();
  });

  it("shows nav item labels by default", async () => {
    renderShellWithRoutes();
    expect(await screen.findByRole("link", { name: "Zero-Downtime Migration" })).toBeInTheDocument();
  });

  // Direct regression test for the actual ask: the sidebar collapses to
  // icon-only — the link's own accessible name (via sr-only text) must
  // still say the full label even though nothing is visibly printed,
  // so this doesn't just check "the visible text disappeared" but that
  // the link stays genuinely usable/labeled for assistive tech too.
  it("collapses to icon-only when the collapse toggle is clicked, keeping accessible names intact", async () => {
    const user = userEvent.setup();
    renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /collapse sidebar/i }));

    const link = screen.getByRole("link", { name: "Zero-Downtime Migration" });
    expect(link).toBeInTheDocument();
    // The visible <span> text is gone once collapsed — only the sr-only
    // fallback (which supplies the accessible name checked above) remains.
    expect(link.querySelector("span:not(.sr-only)")).not.toBeInTheDocument();
  });

  it("expands again when the toggle is clicked a second time", async () => {
    const user = userEvent.setup();
    renderShellWithRoutes();

    const toggle = await screen.findByRole("button", { name: /collapse sidebar/i });
    await user.click(toggle);
    await user.click(await screen.findByRole("button", { name: /expand sidebar/i }));

    const link = screen.getByRole("link", { name: "Zero-Downtime Migration" });
    expect(link.querySelector("span:not(.sr-only)")).toBeInTheDocument();
  });

  it("persists the collapsed state across a remount", async () => {
    const user = userEvent.setup();
    const { unmount } = renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /collapse sidebar/i }));
    unmount();

    renderShellWithRoutes();
    expect(await screen.findByRole("button", { name: /expand sidebar/i })).toBeInTheDocument();
  });

  // Direct regression test for the actual bug report: the collapse
  // toggle used to sit at the BOTTOM of the nav list, requiring a
  // scroll to reach on a long page. It must now come BEFORE the first
  // nav item in the sidebar's own DOM order, so it's always visible at
  // the top without scrolling — combined with the sidebar's own fixed
  // height (see the outer h-screen/overflow-hidden layout), it never
  // needs to be found by scrolling at all.
  it("places the collapse toggle before the nav items, not after", async () => {
    renderShellWithRoutes();
    const nav = await screen.findByRole("navigation", { name: /main navigation/i });

    const toggle = within(nav).getByRole("button", { name: /collapse sidebar/i });
    const firstNavLink = within(nav).getByRole("link", { name: "Zero-Downtime Migration" });

    // DOCUMENT_POSITION_FOLLOWING means "toggle comes before this node."
    // eslint-disable-next-line no-bitwise
    expect(toggle.compareDocumentPosition(firstNavLink) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

describe("Shell — user menu", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "admin@b.com", role: "admin" });
    window.localStorage.clear();
  });

  it("hides Switch user / Sign out until the user button is clicked", async () => {
    renderShellWithRoutes();
    await screen.findByText("pgArchiMigrator");

    expect(screen.queryByRole("menuitem", { name: /sign out/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /switch user/i })).not.toBeInTheDocument();
  });

  it("opens the menu on click, showing both Switch user and Sign out", async () => {
    const user = userEvent.setup();
    renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /admin/i }));

    expect(await screen.findByRole("menuitem", { name: /switch user/i })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /sign out/i })).toBeInTheDocument();
  });

  it("closes the menu when clicking outside it", async () => {
    const user = userEvent.setup();
    renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /admin/i }));
    await screen.findByRole("menuitem", { name: /sign out/i });

    await user.click(screen.getByText("pgArchiMigrator"));
    expect(screen.queryByRole("menuitem", { name: /sign out/i })).not.toBeInTheDocument();
  });

  it("closes the menu on Escape", async () => {
    const user = userEvent.setup();
    renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /admin/i }));
    await screen.findByRole("menuitem", { name: /sign out/i });

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menuitem", { name: /sign out/i })).not.toBeInTheDocument();
  });

  // Direct regression test for both menu items calling the same
  // underlying logout action — see Shell.tsx's own comment on why
  // "Switch user" and "Sign out" are two labels for one mechanism.
  it("calls the API logout when 'Switch user' is clicked, same as Sign out", async () => {
    vi.mocked(api.logout).mockResolvedValue({ status: "ok" });
    const user = userEvent.setup();
    renderShellWithRoutes();

    await user.click(await screen.findByRole("button", { name: /admin/i }));
    await user.click(await screen.findByRole("menuitem", { name: /switch user/i }));

    expect(api.logout).toHaveBeenCalled();
  });
});
