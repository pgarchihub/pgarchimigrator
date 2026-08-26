import { act, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";

vi.mock("./api", () => {
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

import { api, ApiError, setUnauthorizedHandler } from "./api";
import { AuthProvider, isUnauthorized, useAuth } from "./auth";

function wrapper({ children }: { children: ReactNode }) {
  return <AuthProvider>{children}</AuthProvider>;
}

describe("useAuth / AuthProvider", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset();
    vi.mocked(api.login).mockReset();
    vi.mocked(api.logout).mockReset();
  });

  it("starts in 'loading' and moves to 'authenticated' when /me succeeds", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "a@b.com", role: "operator" });
    const { result } = renderHook(() => useAuth(), { wrapper });

    expect(result.current.status).toBe("loading");
    await waitFor(() => expect(result.current.status).toBe("authenticated"));
    expect(result.current.user?.email).toBe("a@b.com");
  });

  it("moves to 'anonymous' when /me fails (no session)", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    const { result } = renderHook(() => useAuth(), { wrapper });

    await waitFor(() => expect(result.current.status).toBe("anonymous"));
    expect(result.current.user).toBeNull();
  });

  describe("hasRole", () => {
    it("ranks admin above operator above viewer", async () => {
      vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
      const { result } = renderHook(() => useAuth(), { wrapper });
      await waitFor(() => expect(result.current.status).toBe("authenticated"));

      expect(result.current.hasRole("viewer")).toBe(true);
      expect(result.current.hasRole("operator")).toBe(true);
      expect(result.current.hasRole("admin")).toBe(false);
    });

    it("returns false for every role when there is no user", async () => {
      vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
      const { result } = renderHook(() => useAuth(), { wrapper });
      await waitFor(() => expect(result.current.status).toBe("anonymous"));

      expect(result.current.hasRole("viewer")).toBe(false);
    });
  });

  // Regression test for a real bug found while writing this suite: logout()
  // originally had no catch (only try/finally), so a failed api.logout()
  // call would clear local state in `finally` but then RE-THROW, breaking
  // any caller (e.g. Shell's handleLogout) that chains a navigate() right
  // after awaiting logout(). Fixed by swallowing the error explicitly.
  it("clears local state AND does not throw when the network call fails", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "a@b.com", role: "admin" });
    vi.mocked(api.logout).mockRejectedValue(new Error("network down"));
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("authenticated"));

    // If logout() re-throws after clearing state (the original bug this
    // test guards against), this await rejects and fails the test on its
    // own — no special "does not throw" matcher needed, since
    // `.resolves.not.toThrow()` isn't valid on an already-awaited value
    // anyway (toThrow expects a function to invoke, not a resolved result).
    await act(async () => {
      await result.current.logout();
    });

    expect(result.current.status).toBe("anonymous");
    expect(result.current.user).toBeNull();
  });

  it("login() sets the user and status on success", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    vi.mocked(api.login).mockResolvedValue({ id: "u2", email: "new@b.com", role: "viewer" });
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("anonymous"));

    await act(async () => {
      await result.current.login("new@b.com", "pw");
    });

    expect(result.current.status).toBe("authenticated");
    expect(result.current.user?.email).toBe("new@b.com");
  });
});

// These tests cover the mechanism that makes a 401 mid-use actually DO
// something — see setUnauthorizedHandler's doc comment in api.ts. The
// registered handler is captured from setUnauthorizedHandler's own mock
// calls and invoked directly, simulating exactly what api.ts's request()
// does internally when some other screen's API call gets back a 401.
describe("AuthProvider's unauthorized-handler registration", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset();
    vi.mocked(setUnauthorizedHandler).mockReset();
  });

  function lastRegisteredHandler(): (() => void) | undefined {
    const calls = vi.mocked(setUnauthorizedHandler).mock.calls;
    // Array.prototype.at() requires ES2022+ (this project targets
    // ES2020 — see tsconfig.app.json), so plain indexing is used instead.
    return calls[calls.length - 1]?.[0] ?? undefined;
  }

  it("registers a handler on mount and unregisters (passes null) on unmount", () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    const { unmount } = renderHook(() => useAuth(), { wrapper });
    expect(setUnauthorizedHandler).toHaveBeenCalledWith(expect.any(Function));

    unmount();
    expect(setUnauthorizedHandler).toHaveBeenLastCalledWith(null);
  });

  it("sets sessionExpired when a 401 arrives for a PREVIOUSLY authenticated user", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "a@b.com", role: "admin" });
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("authenticated"));

    act(() => lastRegisteredHandler()?.());

    expect(result.current.sessionExpired).toBe(true);
    expect(result.current.status).toBe("anonymous");
    expect(result.current.user).toBeNull();
  });

  it("does NOT set sessionExpired when the 401 comes from an already-anonymous context (e.g. the initial /me check with no cookie)", async () => {
    vi.mocked(api.me).mockRejectedValue(new ApiError(401, "unauthenticated"));
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("anonymous"));

    act(() => lastRegisteredHandler()?.());

    // Nothing "expired" — there was never a real session to begin with.
    expect(result.current.sessionExpired).toBe(false);
  });

  it("clearSessionExpired resets the flag", async () => {
    vi.mocked(api.me).mockResolvedValue({ id: "u1", email: "a@b.com", role: "admin" });
    const { result } = renderHook(() => useAuth(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("authenticated"));

    act(() => lastRegisteredHandler()?.());
    expect(result.current.sessionExpired).toBe(true);

    act(() => result.current.clearSessionExpired());
    expect(result.current.sessionExpired).toBe(false);
  });
});

describe("isUnauthorized", () => {
  it("returns true for a 401 ApiError", () => {
    expect(isUnauthorized(new ApiError(401, "unauthenticated"))).toBe(true);
  });

  it("returns false for other ApiError statuses", () => {
    expect(isUnauthorized(new ApiError(500, "server error"))).toBe(false);
  });

  it("returns false for a non-ApiError value", () => {
    expect(isUnauthorized(new Error("boom"))).toBe(false);
  });
});
