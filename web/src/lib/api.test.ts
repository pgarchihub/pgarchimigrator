import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, setUnauthorizedHandler } from "./api";

function mockFetchOnce(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: status >= 200 && status < 300,
      status,
      json: async () => body,
    }),
  );
}

function mockFetchOnceNonJSON(status: number) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: status >= 200 && status < 300,
      status,
      json: async () => {
        throw new SyntaxError("Unexpected token < in JSON");
      },
    }),
  );
}

describe("api request error handling", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns the parsed body on success", async () => {
    mockFetchOnce(200, { id: "u1", email: "a@b.com", role: "admin" });
    const user = await api.me();
    expect(user.email).toBe("a@b.com");
  });

  it("throws an ApiError with the backend's own message when the body has one", async () => {
    mockFetchOnce(400, { error: "table is required" });
    await expect(api.me()).rejects.toMatchObject(
      new ApiError(400, "table is required"),
    );
  });

  it("falls back to a generic message when the error body isn't JSON", async () => {
    mockFetchOnceNonJSON(500);
    await expect(api.me()).rejects.toMatchObject(
      new ApiError(500, "request failed with status 500"),
    );
  });

  it("carries the HTTP status on the thrown ApiError", async () => {
    mockFetchOnce(401, { error: "unauthenticated" });
    try {
      await api.me();
      expect.unreachable("expected api.me() to throw");
    } catch (err) {
      expect(err).toBeInstanceOf(ApiError);
      expect((err as ApiError).status).toBe(401);
    }
  });
});

// rollbackMigration deliberately bypasses the generic request() helper —
// see its doc comment in api.ts: internal/api's handleRollback returns a
// full MigrationReport body (not an {error} shape) on BOTH success (200)
// AND a refused rollback (422, e.g. "window expired"). These tests are a
// regression guard for that specific, easy-to-get-wrong behavior.
describe("api.rollbackMigration", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns the report on a successful rollback (200)", async () => {
    const report = { JobID: "job-1", CurrentPhase: "ABORTED", Terminal: true };
    vi.mocked(fetch).mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => report,
    } as Response);

    const result = await api.rollbackMigration("job-1");
    expect(result.JobID).toBe("job-1");
    expect(result.CurrentPhase).toBe("ABORTED");
  });

  it("returns the report (not a thrown error) on a REFUSED rollback (422)", async () => {
    const report = {
      JobID: "job-2",
      CurrentPhase: "COMPLETED",
      Terminal: true,
      LastError: "ddlflow: refusing to roll back a COMPLETED migration",
    };
    vi.mocked(fetch).mockResolvedValue({
      ok: false,
      status: 422,
      json: async () => report,
    } as Response);

    const result = await api.rollbackMigration("job-2");
    expect(result.LastError).toContain("refusing to roll back");
  });

  it("throws an ApiError for a genuine failure (e.g. 404 job not found)", async () => {
    vi.mocked(fetch).mockResolvedValue({
      ok: false,
      status: 404,
      json: async () => ({ error: "job not found" }),
    } as Response);

    await expect(api.rollbackMigration("does-not-exist")).rejects.toMatchObject(
      new ApiError(404, "job not found"),
    );
  });
});

// startMigration deliberately bypasses the generic request() helper —
// mirrors api.rollbackMigration's identical reasoning just above: a real
// bug found via manual testing, where a migration that ran but FAILED
// (422 — a job was created, just didn't succeed) was thrown as a generic
// ApiError, discarding the report (including its JobID) entirely — so
// NewMigration.tsx's handleSubmit could never navigate the caller to the
// failed job's own detail page to show WHY it failed, even though a
// real, inspectable job existed the whole time.
describe("api.getMigration", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("does not append a query parameter when measureImpact is omitted (defaults to false)", async () => {
    const fetchSpy = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ JobID: "job-1" }) });
    vi.stubGlobal("fetch", fetchSpy);

    await api.getMigration("job-1");

    expect(fetchSpy).toHaveBeenCalledWith("/api/migrations/job-1", expect.anything());
  });

  it("does not append a query parameter when measureImpact is explicitly false", async () => {
    const fetchSpy = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ JobID: "job-1" }) });
    vi.stubGlobal("fetch", fetchSpy);

    await api.getMigration("job-1", false);

    expect(fetchSpy).toHaveBeenCalledWith("/api/migrations/job-1", expect.anything());
  });

  // Direct regression test for the opt-in contract itself: the query
  // parameter must be present ONLY when explicitly requested — see
  // internal/api's attachImpactMeasurement doc comment for why this one
  // trust-layer indicator, unlike the others, isn't always computed.
  it("appends ?measureImpact=true when explicitly requested", async () => {
    const fetchSpy = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ JobID: "job-1" }) });
    vi.stubGlobal("fetch", fetchSpy);

    await api.getMigration("job-1", true);

    expect(fetchSpy).toHaveBeenCalledWith("/api/migrations/job-1?measureImpact=true", expect.anything());
  });
});

describe("api.startMigration", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const minimalRequest = { schema: "public", table: "orders", operation: "ADD_COLUMN" as const };

  it("returns the report on a successful start (200)", async () => {
    const report = { JobID: "job-1", CurrentPhase: "COMPLETED", Terminal: true };
    vi.mocked(fetch).mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => report,
    } as Response);

    const result = await api.startMigration(minimalRequest);
    expect(result.JobID).toBe("job-1");
  });

  it("returns the report on a successful start (201 Created)", async () => {
    const report = { JobID: "job-2", CurrentPhase: "PREFLIGHT", Terminal: false };
    vi.mocked(fetch).mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => report,
    } as Response);

    const result = await api.startMigration(minimalRequest);
    expect(result.JobID).toBe("job-2");
  });

  it("returns the report (not a thrown error) when the migration ran but FAILED (422)", async () => {
    const report = {
      JobID: "job-3",
      CurrentPhase: "FAILED",
      Terminal: true,
      Failed: true,
      LastError: "initial sync failed: duplicate key value violates unique constraint",
    };
    vi.mocked(fetch).mockResolvedValue({
      ok: false,
      status: 422,
      json: async () => report,
    } as Response);

    const result = await api.startMigration(minimalRequest);
    expect(result.JobID).toBe("job-3");
    expect(result.LastError).toContain("duplicate key");
  });

  it("throws an ApiError for a genuinely invalid request (e.g. 400 bad request, no job ever created)", async () => {
    vi.mocked(fetch).mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({ error: "column is required for ADD_COLUMN" }),
    } as Response);

    await expect(api.startMigration(minimalRequest)).rejects.toMatchObject(
      new ApiError(400, "column is required for ADD_COLUMN"),
    );
  });
});

// setUnauthorizedHandler is how lib/auth.tsx learns a session died
// mid-use — see its doc comment in api.ts. These tests are a regression
// guard for a real gap: this mechanism was added specifically because
// isUnauthorized() existed but nothing ever called it, so a 401 mid-use
// just showed a confusing inline error instead of returning the user to
// the login screen.
describe("setUnauthorizedHandler", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    setUnauthorizedHandler(null); // don't leak a handler into other test files
  });

  it("is called on a 401 from the generic request() path", async () => {
    mockFetchOnce(401, { error: "unauthenticated" });
    const handler = vi.fn();
    setUnauthorizedHandler(handler);

    await expect(api.me()).rejects.toBeInstanceOf(ApiError);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("is called on a 401 from rollbackMigration's own fetch path (it bypasses request())", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ error: "unauthenticated" }) }),
    );
    const handler = vi.fn();
    setUnauthorizedHandler(handler);

    await expect(api.rollbackMigration("job-1")).rejects.toBeInstanceOf(ApiError);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("is NOT called for other error statuses (400, 404, 500)", async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);

    mockFetchOnce(400, { error: "bad request" });
    await expect(api.me()).rejects.toBeInstanceOf(ApiError);
    mockFetchOnce(404, { error: "not found" });
    await expect(api.me()).rejects.toBeInstanceOf(ApiError);
    mockFetchOnce(500, { error: "server error" });
    await expect(api.me()).rejects.toBeInstanceOf(ApiError);

    expect(handler).not.toHaveBeenCalled();
  });

  it("is not called at all on success", async () => {
    mockFetchOnce(200, { id: "u1", email: "a@b.com", role: "admin" });
    const handler = vi.fn();
    setUnauthorizedHandler(handler);

    await api.me();
    expect(handler).not.toHaveBeenCalled();
  });

  it("passing null unregisters the handler", async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    setUnauthorizedHandler(null);

    mockFetchOnce(401, { error: "unauthenticated" });
    await expect(api.me()).rejects.toBeInstanceOf(ApiError);
    expect(handler).not.toHaveBeenCalled();
  });
});
