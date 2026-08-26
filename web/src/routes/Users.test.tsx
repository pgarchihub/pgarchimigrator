import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    listUsers: vi.fn(),
    createUser: vi.fn(),
    deleteUser: vi.fn(),
    updateUserRole: vi.fn(),
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
import Users from "./Users";

function renderUsers() {
  return render(
    <AuthProvider>
      <Users />
    </AuthProvider>,
  );
}

describe("Users accessibility", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u-admin", email: "admin@b.com", role: "admin" });
    vi.mocked(api.listUsers).mockReset();
  });

  // A screen reader user tabbing through a table of "Remove" buttons with
  // no distinguishing accessible name would hear "Remove, Remove, Remove…"
  // with no way to tell which row each one belongs to — this is a
  // regression guard for exactly that.
  it("gives each Remove button a distinct accessible name naming its user", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
      { id: "u2", email: "bob@b.com", role: "operator" },
    ]);
    renderUsers();

    const removeAlice = await screen.findByRole("button", { name: "Remove alice@b.com" });
    const removeBob = await screen.findByRole("button", { name: "Remove bob@b.com" });
    expect(removeAlice).toBeInTheDocument();
    expect(removeBob).toBeInTheDocument();
    // Distinct names, not just distinct DOM nodes with identical text.
    expect(removeAlice.getAttribute("aria-label")).not.toBe(removeBob.getAttribute("aria-label"));
  });

  it("does not show a Remove button for the currently signed-in user", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
    ]);
    renderUsers();

    await screen.findByRole("button", { name: "Remove alice@b.com" });
    expect(screen.queryByRole("button", { name: /remove admin@b.com/i })).not.toBeInTheDocument();
  });

  it("labels the users table for assistive tech", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([{ id: "u-admin", email: "admin@b.com", role: "admin" }]);
    renderUsers();
    await waitFor(() => expect(screen.getByRole("table", { name: "Users" })).toBeInTheDocument());
  });
});

describe("Users role management", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u-admin", email: "admin@b.com", role: "admin" });
    vi.mocked(api.listUsers).mockReset();
    vi.mocked(api.updateUserRole).mockReset();
  });

  it("shows an editable role dropdown for other users, not a read-only badge", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
    ]);
    renderUsers();

    const roleSelect = await screen.findByRole("combobox", { name: "Role for alice@b.com" });
    expect((roleSelect as HTMLSelectElement).value).toBe("viewer");
  });

  it("shows the current user's own role as a read-only badge, not an editable dropdown (self-role-change is refused server-side)", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([{ id: "u-admin", email: "admin@b.com", role: "admin" }]);
    renderUsers();

    await screen.findByText("admin@b.com");
    expect(screen.queryByRole("combobox", { name: "Role for admin@b.com" })).not.toBeInTheDocument();
  });

  it("calls api.updateUserRole and refreshes the list when a role is changed", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
    ]);
    vi.mocked(api.updateUserRole).mockResolvedValue({ status: "updated" });
    const user = userEvent.setup();
    renderUsers();

    const roleSelect = await screen.findByRole("combobox", { name: "Role for alice@b.com" });
    await user.selectOptions(roleSelect, "operator");

    await waitFor(() => expect(api.updateUserRole).toHaveBeenCalledWith("u1", "operator"));
    // The list is reloaded after a successful change — listUsers should
    // have been called again (once on mount, once after the update).
    await waitFor(() => expect(api.listUsers).toHaveBeenCalledTimes(2));
  });

  it("shows an error message and does not crash when the role update is refused", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
    ]);
    vi.mocked(api.updateUserRole).mockRejectedValue(new ApiError(400, "invalid role"));
    const user = userEvent.setup();
    renderUsers();

    const roleSelect = await screen.findByRole("combobox", { name: "Role for alice@b.com" });
    await user.selectOptions(roleSelect, "admin");

    expect(await screen.findByText("invalid role")).toBeInTheDocument();
  });

  // Distinct accessible names per select — the same discipline as the
  // Remove buttons above: several role dropdowns with identical visible
  // option text ("viewer", "operator", "admin") need distinguishable
  // labels for anyone navigating by screen reader.
  it("gives each role dropdown a distinct accessible name naming its user", async () => {
    vi.mocked(api.listUsers).mockResolvedValue([
      { id: "u-admin", email: "admin@b.com", role: "admin" },
      { id: "u1", email: "alice@b.com", role: "viewer" },
      { id: "u2", email: "bob@b.com", role: "operator" },
    ]);
    renderUsers();

    const roleAlice = await screen.findByRole("combobox", { name: "Role for alice@b.com" });
    const roleBob = await screen.findByRole("combobox", { name: "Role for bob@b.com" });
    expect(roleAlice.getAttribute("aria-label")).not.toBe(roleBob.getAttribute("aria-label"));
  });
});
