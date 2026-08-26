import type {
  Analytics,
  ColumnInfo,
  ConnectionInfo,
  CurrentUser,
  ManagedUser,
  MigrationReport,
  PreviewReport,
  Role,
  SampleRowsResult,
  SetupRequiredResponse,
  StartMigrationRequest,
  StrategyMatrix,
  TableStats,
  WriteLoadEstimate,
} from "./types";

// ApiError carries the exact message the backend's writeError() produced
// (see internal/api/server.go) — surfaced as-is in the UI rather than a
// generic "something went wrong", since the backend's error text is
// already written to be read by a human operator.
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

// unauthorizedHandler is how lib/auth.tsx's AuthProvider learns that a
// session died mid-use (a 401 from ANY API call, on ANY screen), without
// api.ts importing React/auth.tsx directly (which would create a
// circular dependency: auth.tsx already imports from api.ts). AuthProvider
// registers a handler on mount that clears local auth state; once that
// state flips to "anonymous", App.tsx's existing RequireAuth redirect
// logic takes over automatically — no per-screen changes needed to get a
// consistent "kicked back to /login" experience everywhere, instead of
// every screen's catch block quietly showing a confusing inline error
// while the UI still looks logged in.
let unauthorizedHandler: (() => void) | null = null;

export function setUnauthorizedHandler(handler: (() => void) | null) {
  unauthorizedHandler = handler;
}

function notifyUnauthorized(status: number) {
  if (status === 401) unauthorizedHandler?.();
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: init?.body ? { "Content-Type": "application/json" } : undefined,
    ...init,
  });

  if (!res.ok) {
    let message = `request failed with status ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // body wasn't JSON — keep the generic message
    }
    notifyUnauthorized(res.status);
    throw new ApiError(res.status, message);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  // --- Auth ---
  login: (email: string, password: string) =>
    request<CurrentUser>("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    }),
  logout: () => request<{ status: string }>("/api/auth/logout", { method: "POST" }),
  me: () => request<CurrentUser>("/api/auth/me"),
  // setupRequired/setup back the first-run wizard — see
  // internal/api's handleSetupRequired/handleSetup doc comments. Both are
  // reachable without a session, since by definition nothing can be
  // authenticated yet on a deployment that hasn't been set up.
  setupRequired: () => request<SetupRequiredResponse>("/api/setup-required"),
  setup: (email: string, password: string) =>
    request<CurrentUser>("/api/setup", { method: "POST", body: JSON.stringify({ email, password }) }),
  getVersion: () => request<{ version: string }>("/api/version"),

  // --- Migrations ---
  listMigrations: () => request<MigrationReport[]>("/api/migrations"),
  getAnalytics: () => request<Analytics>("/api/analytics"),
  getMigration: (id: string, measureImpact = false) =>
    request<MigrationReport>(
      `/api/migrations/${encodeURIComponent(id)}${measureImpact ? "?measureImpact=true" : ""}`,
    ),
  // Deliberately does NOT use the generic request() helper — mirrors
  // rollbackMigration's identical reasoning just below: internal/api's
  // handleStartMigration returns a full MigrationReport body (not an
  // {error} shape) on BOTH success (200/201) and a migration that ran
  // but failed (422 — a job WAS created, Execute() just didn't succeed;
  // see that handler's own doc comment). Treating 422 as a generic
  // thrown error discarded the report entirely — including its JobID —
  // so the caller (NewMigration.tsx) could never navigate to the job's
  // own detail page to show WHY it failed, even though a real,
  // inspectable job existed the whole time. Only a genuinely invalid
  // request (400, no job ever created) should actually throw.
  startMigration: async (body: StartMigrationRequest): Promise<MigrationReport> => {
    const res = await fetch("/api/migrations", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (res.status === 200 || res.status === 201 || res.status === 422) {
      return (await res.json()) as MigrationReport;
    }
    let message = `request failed with status ${res.status}`;
    try {
      const errBody = (await res.json()) as { error?: string };
      if (errBody.error) message = errBody.error;
    } catch {
      // body wasn't JSON — keep the generic message
    }
    notifyUnauthorized(res.status);
    throw new ApiError(res.status, message);
  },
  previewMigration: (body: StartMigrationRequest) =>
    request<PreviewReport>("/api/migrations/preview", { method: "POST", body: JSON.stringify(body) }),
  // rollbackMigration deliberately does NOT use the generic request()
  // helper: internal/api's handleRollback returns a full MigrationReport
  // body (not an {error} shape) on BOTH success (200) and a refused
  // rollback (422, e.g. "window expired" or "refuses to roll back a
  // COMPLETED job") — see that handler's doc comment. Treating 422 as a
  // generic failure would discard the report's LastError, which is
  // exactly the human-readable explanation the UI needs to show.
  rollbackMigration: async (id: string): Promise<MigrationReport> => {
    const res = await fetch(`/api/migrations/${encodeURIComponent(id)}/rollback`, {
      method: "POST",
      credentials: "same-origin",
    });
    if (res.status === 200 || res.status === 422) {
      return (await res.json()) as MigrationReport;
    }
    let message = `request failed with status ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // body wasn't JSON — keep the generic message
    }
    notifyUnauthorized(res.status);
    throw new ApiError(res.status, message);
  },

  // --- Sweep ---
  sweep: () => request<unknown>("/api/sweep", { method: "POST" }),

  // --- Users (admin only) ---
  listUsers: () => request<ManagedUser[]>("/api/users"),
  createUser: (email: string, password: string, role: Role) =>
    request<ManagedUser>("/api/users", { method: "POST", body: JSON.stringify({ email, password, role }) }),
  deleteUser: (id: string) => request<{ status: string }>(`/api/users/${encodeURIComponent(id)}`, { method: "DELETE" }),
  updateUserRole: (id: string, role: Role) =>
    request<{ status: string }>(`/api/users/${encodeURIComponent(id)}/role`, {
      method: "PATCH",
      body: JSON.stringify({ role }),
    }),

  // --- Catalog browsing (New Migration screen's schema/table/column
  // dropdowns) — see internal/catalog's package doc comment. ---
  listSchemas: () => request<string[]>("/api/schemas"),
  listTables: (schema: string) => request<string[]>(`/api/schemas/${encodeURIComponent(schema)}/tables`),
  listColumns: (schema: string, table: string) =>
    request<ColumnInfo[]>(`/api/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}/columns`),
  sampleRows: (schema: string, table: string) =>
    request<SampleRowsResult>(
      `/api/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}/sample`,
    ),
  getTableStats: (schema: string, table: string) =>
    request<TableStats>(`/api/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}/stats`),
  getConnectionInfo: () => request<ConnectionInfo>("/api/connection"),
  getStrategyMatrix: () => request<StrategyMatrix>("/api/strategy-matrix"),
  // Blocks for ~10 seconds (see internal/api's writeLoadSampleDuration)
  // — only called when the New Migration screen's opt-in "check current
  // write load" step is explicitly triggered, never automatically.
  estimateWriteLoad: () => request<WriteLoadEstimate>("/api/migrations/estimate-write-load", { method: "POST" }),
};
