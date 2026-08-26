import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
vi.mock("../lib/api", () => {
  class MockApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
      this.name = "ApiError";
    }
  }
  return {
    api: {
      me: vi.fn(),
      login: vi.fn(),
      logout: vi.fn(),
      setupRequired: vi.fn().mockResolvedValue({ required: false }),
      getVersion: vi.fn().mockResolvedValue({ version: "test" }),
    },
    ApiError: MockApiError,
    setUnauthorizedHandler: vi.fn(),
  };
});

import { api, ApiError, setUnauthorizedHandler } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import Login from "./Login";

// renderLogin awaits a tick after mounting so AuthProvider's initial
// api.me() effect (mocked to reject immediately, below) settles inside a
// tracked act() scope before the test proceeds. Without this, that
// rejection can resolve later — right as afterEach's cleanup() unmounts
// the tree — producing a harmless but noisy React "not wrapped in
// act(...)" warning on every test in this file.
async function renderLogin() {
  const result = render(
    <AuthProvider>
      <MemoryRouter initialEntries={["/login"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Login />
      </MemoryRouter>
    </AuthProvider>,
  );
  await act(async () => {});
  return result;
}

describe("Login", () => {
  beforeEach(() => {
    // Every render mounts a real AuthProvider, which fires api.me() on
    // mount — mocked to "no session" here so it doesn't interfere with
    // (or race against) each test's own login-flow assertions.
    vi.mocked(api.me).mockReset().mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.login).mockReset();
  });

  it("renders email and password fields and a submit button", async () => {
    await renderLogin();
    expect(await screen.findByLabelText(/email/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
  });

  it("shows the backend's own error message on a failed login", async () => {
    vi.mocked(api.login).mockRejectedValue(new ApiError(401, "invalid credentials"));
    const user = userEvent.setup();
    await renderLogin();

    await user.type(await screen.findByLabelText(/email/i), "wrong@b.com");
    await user.type(screen.getByLabelText(/password/i), "wrongpass");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent("invalid credentials");
  });

  it("calls api.login with exactly what the user typed", async () => {
    vi.mocked(api.login).mockResolvedValue({ id: "u1", email: "a@b.com", role: "viewer" });
    const user = userEvent.setup();
    await renderLogin();

    await user.type(await screen.findByLabelText(/email/i), "a@b.com");
    await user.type(screen.getByLabelText(/password/i), "secret123");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(api.login).toHaveBeenCalledWith("a@b.com", "secret123"));
  });

  it("disables the submit button and shows a busy label while submitting", async () => {
    let resolveLogin!: (value: { id: string; email: string; role: "viewer" }) => void;
    vi.mocked(api.login).mockReturnValue(
      new Promise((resolve) => {
        resolveLogin = resolve;
      }),
    );
    const user = userEvent.setup();
    await renderLogin();

    await user.type(await screen.findByLabelText(/email/i), "a@b.com");
    await user.type(screen.getByLabelText(/password/i), "secret123");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    const busyButton = await screen.findByRole("button", { name: /signing in/i });
    expect(busyButton).toBeDisabled();

    // Wrapped in act(): resolving this drives Login's handleSubmit to
    // completion (setSubmitting(false), navigate(...)) — none of which
    // the test otherwise awaits, so without this wrapper those updates
    // land after the test function returns, right as cleanup() unmounts
    // the tree.
    await act(async () => {
      resolveLogin({ id: "u1", email: "a@b.com", role: "viewer" });
    });
  });

  // Regression coverage for the mechanism that makes a 401 mid-use
  // actually visible to the person, not just a silent redirect — see
  // api.ts's setUnauthorizedHandler and auth.tsx's sessionExpired.
  it("shows a 'session expired' message when a previously authenticated session dies mid-use", async () => {
    // Simulate a real session existing at mount (unlike this file's
    // default beforeEach, which mocks "never logged in") — sessionExpired
    // only makes sense to become true when there WAS a session to lose.
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "a@b.com", role: "viewer" });
    await renderLogin();

    // Grab the handler AuthProvider registered and invoke it directly —
    // simulating exactly what api.ts does internally when some other
    // screen's API call gets back a 401 while this session was active.
    const registeredHandlerCalls = vi.mocked(setUnauthorizedHandler).mock.calls;
    const registeredHandler = registeredHandlerCalls[registeredHandlerCalls.length - 1]?.[0];
    expect(registeredHandler).toBeTypeOf("function");
    await act(async () => {
      registeredHandler?.();
    });

    expect(await screen.findByText("Your session has expired. Please sign in again.")).toBeInTheDocument();
  });

  it("does not show the session-expired message on an ordinary (never logged in) visit", async () => {
    // beforeEach already mocks api.me() to reject (no session) — the
    // default, most common case: someone just navigating to /login
    // directly, not someone who got bounced here after a 401.
    await renderLogin();
    expect(screen.queryByText(/session has expired/i)).not.toBeInTheDocument();
  });
});
