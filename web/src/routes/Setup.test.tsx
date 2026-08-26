import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../lib/api", () => ({
  api: {
    me: vi.fn(),
    setupRequired: vi.fn(),
    setup: vi.fn(),
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
import Setup from "./Setup";

function renderSetup() {
  return render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/setup"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Setup />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("Setup", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.setupRequired).mockReset().mockResolvedValue({ required: true });
    vi.mocked(api.setup).mockReset();
  });

  it("renders email, password, and confirm-password fields", async () => {
    renderSetup();
    expect(await screen.findByLabelText(/email/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/^password/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/confirm password/i)).toBeInTheDocument();
  });

  it("refuses to submit when the passwords don't match, without calling the API at all", async () => {
    const user = userEvent.setup();
    renderSetup();

    await user.type(await screen.findByLabelText(/email/i), "founder@company.com");
    await user.type(screen.getByLabelText(/^password/i), "a-strong-password");
    await user.type(screen.getByLabelText(/confirm password/i), "a-different-password");
    await user.click(screen.getByRole("button", { name: /create account/i }));

    expect(await screen.findByText(/don't match/i)).toBeInTheDocument();
    expect(api.setup).not.toHaveBeenCalled();
  });

  it("refuses a too-short password client-side before calling the API", async () => {
    const user = userEvent.setup();
    renderSetup();

    await user.type(await screen.findByLabelText(/email/i), "founder@company.com");
    await user.type(screen.getByLabelText(/^password/i), "short");
    await user.type(screen.getByLabelText(/confirm password/i), "short");
    await user.click(screen.getByRole("button", { name: /create account/i }));

    expect(await screen.findByText(/at least 8 characters/i)).toBeInTheDocument();
    expect(api.setup).not.toHaveBeenCalled();
  });

  it("calls api.setup with the entered credentials when the form is valid", async () => {
    vi.mocked(api.setup).mockResolvedValue({ id: "u1", email: "founder@company.com", role: "admin" });
    const user = userEvent.setup();
    renderSetup();

    await user.type(await screen.findByLabelText(/email/i), "founder@company.com");
    await user.type(screen.getByLabelText(/^password/i), "a-strong-password");
    await user.type(screen.getByLabelText(/confirm password/i), "a-strong-password");
    await user.click(screen.getByRole("button", { name: /create account/i }));

    await waitFor(() => expect(api.setup).toHaveBeenCalledWith("founder@company.com", "a-strong-password"));
  });

  it("shows the backend's own error message when setup is refused (e.g. already bootstrapped)", async () => {
    vi.mocked(api.setup).mockRejectedValue(new ApiError(409, "this deployment is already set up — please sign in instead"));
    const user = userEvent.setup();
    renderSetup();

    await user.type(await screen.findByLabelText(/email/i), "founder@company.com");
    await user.type(screen.getByLabelText(/^password/i), "a-strong-password");
    await user.type(screen.getByLabelText(/confirm password/i), "a-strong-password");
    await user.click(screen.getByRole("button", { name: /create account/i }));

    expect(await screen.findByText(/already set up/i)).toBeInTheDocument();
  });
});
