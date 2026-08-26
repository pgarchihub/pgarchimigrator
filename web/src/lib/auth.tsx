import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { api, ApiError, setUnauthorizedHandler } from "./api";
import type { CurrentUser, Role } from "./types";

interface AuthContextValue {
  user: CurrentUser | null;
  status: "loading" | "authenticated" | "anonymous";
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  hasRole: (min: Role) => boolean;
  // sessionExpired is true only when a PREVIOUSLY authenticated session
  // died mid-use (a 401 from some API call while the user had a real
  // session) — not for the ordinary "never logged in yet" case. Login.tsx
  // reads this to show "Your session expired, please sign in again"
  // instead of a bare empty form, and clears it once shown.
  sessionExpired: boolean;
  clearSessionExpired: () => void;
  // setupRequired is null until the initial GET /api/setup-required check
  // resolves — App.tsx waits for this (alongside `status`) before
  // deciding whether to route to the first-run setup wizard, the login
  // screen, or the app itself, so nothing flashes the wrong screen for a
  // frame while this is still in flight.
  setupRequired: boolean | null;
  completeSetup: (email: string, password: string) => Promise<void>;
}

const roleRank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<CurrentUser | null>(null);
  const [status, setStatus] = useState<AuthContextValue["status"]>("loading");
  const [sessionExpired, setSessionExpired] = useState(false);
  const [setupRequired, setSetupRequired] = useState<boolean | null>(null);

  useEffect(() => {
    api
      .me()
      .then((u) => {
        setUser(u);
        setStatus("authenticated");
      })
      .catch(() => {
        setUser(null);
        setStatus("anonymous");
      });
  }, []);

  useEffect(() => {
    api
      .setupRequired()
      .then((res) => setSetupRequired(res.required))
      .catch(() => {
        // Fail open to the ordinary login screen rather than getting
        // stuck: a transient network hiccup on this one check shouldn't
        // block access to an already-configured deployment.
        setSetupRequired(false);
      });
  }, []);

  // Registered once, for the lifetime of this provider — see
  // setUnauthorizedHandler's doc comment in api.ts for why this
  // indirection exists. Fires on a 401 from ANY API call, anywhere in the
  // app, including this provider's own api.me() above (harmless there:
  // it just reaches the same "anonymous" state that catch block already
  // sets).
  useEffect(() => {
    setUnauthorizedHandler(() => {
      // The functional setUser form lets us see whether there WAS a
      // logged-in user a moment ago, distinguishing "a real session just
      // died" from "this 401 came from an already-anonymous context"
      // (e.g. api.me() on first load with no cookie yet, or a failed
      // login attempt) — only the former should show a "session expired"
      // message.
      setUser((prevUser) => {
        if (prevUser) setSessionExpired(true);
        return null;
      });
      setStatus("anonymous");
    });
    return () => setUnauthorizedHandler(null);
  }, []);

  const login = useCallback(async (email: string, password: string) => {
    const u = await api.login(email, password);
    setUser(u);
    setStatus("authenticated");
    setSessionExpired(false);
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch {
      // Swallowed deliberately, not just left uncaught: the cookie may
      // already be invalid or gone server-side regardless of this call's
      // outcome, and callers (e.g. Shell's handleLogout) chain a
      // navigate() right after this resolves — re-throwing here would
      // skip that navigate on a network hiccup, leaving the user stuck on
      // a page whose auth state has already been cleared out from under
      // it. Local state is cleared unconditionally in `finally` below
      // either way.
    } finally {
      setUser(null);
      setStatus("anonymous");
    }
  }, []);

  // completeSetup mirrors login()'s state transition exactly — POST
  // /api/setup logs the new admin in immediately server-side (see
  // handleSetup's doc comment), so from the frontend's perspective this
  // IS a login, just one that also flips setupRequired off for good.
  const completeSetup = useCallback(async (email: string, password: string) => {
    const u = await api.setup(email, password);
    setUser(u);
    setStatus("authenticated");
    setSetupRequired(false);
  }, []);

  const hasRole = useCallback(
    (min: Role) => (user ? roleRank[user.role] >= roleRank[min] : false),
    [user],
  );

  const clearSessionExpired = useCallback(() => setSessionExpired(false), []);

  return (
    <AuthContext.Provider
      value={{
        user,
        status,
        login,
        logout,
        hasRole,
        sessionExpired,
        clearSessionExpired,
        setupRequired,
        completeSetup,
      }}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within an AuthProvider");
  return ctx;
}

// isUnauthorized lets call sites distinguish "session expired mid-use"
// (401) from other failures, e.g. to bounce back to the login screen —
// mirrors the vanilla-JS dashboard's handleUnauthorized() helper. Most
// screens no longer need to call this themselves now that api.ts
// notifies AuthProvider directly on every 401 (see
// setUnauthorizedHandler), but it's kept exported for any call site that
// wants to react to a 401 differently in its own UI (e.g. skip showing
// its own inline error banner, since the redirect is already happening).
export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401;
}
