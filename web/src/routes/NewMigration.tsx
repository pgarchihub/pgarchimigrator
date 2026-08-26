import { type DependencyList, type FormEvent, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import type { ColumnInfo, ConnectionInfo, Operation, PreviewReport, SampleRowsResult, StartMigrationRequest, StrategyMatrix, TableStats, WriteLoadEstimate } from "../lib/types";
import { useAuth } from "../lib/auth";
import { formatBytes, formatRowCount } from "../lib/format";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { TextField } from "../ui/TextField";

const OPERATIONS: Operation[] = [
  "ADD_COLUMN",
  "DROP_COLUMN",
  "ALTER_COLUMN_TYPE",
  "ADD_INDEX",
  "DROP_INDEX",
  "SET_NOT_NULL",
  "ADD_CONSTRAINT",
  "RENAME_COLUMN",
];

export interface FormState {
  schema: string;
  table: string;
  operation: Operation;
  column: string;
  type: string;
  default: string;
  volatile_default: boolean;
  strategy_override: string;
  index_name: string;
  constraint_name: string;
  check_expression: string;
  new_column_name: string;
  name: string;
  description: string;
}

export const initialForm: FormState = {
  schema: "public",
  table: "",
  operation: "ADD_COLUMN",
  column: "",
  type: "",
  default: "",
  volatile_default: false,
  strategy_override: "",
  index_name: "",
  constraint_name: "",
  check_expression: "",
  new_column_name: "",
  name: "",
  description: "",
};

function toRequest(f: FormState): StartMigrationRequest {
  return {
    schema: f.schema || undefined,
    table: f.table,
    operation: f.operation,
    column: f.column || undefined,
    type: f.type || undefined,
    default: f.default || undefined,
    volatile_default: f.volatile_default,
    strategy_override: f.strategy_override || undefined,
    index_name: f.index_name || undefined,
    constraint_name: f.constraint_name || undefined,
    check_expression: f.check_expression || undefined,
    new_column_name: f.new_column_name || undefined,
    name: f.name || undefined,
    description: f.description || undefined,
  };
}

// isReadyForPreview mirrors internal/api's buildMigrationRequest
// validation (see server.go) exactly — the same fields it treats as
// required per operation. Kept in sync manually; if that validation ever
// changes, this needs to change with it or the preview panel will either
// fire doomed requests or withhold a preview it could have shown.
export function isReadyForPreview(f: FormState): boolean {
  if (!f.table.trim()) return false;
  switch (f.operation) {
    case "DROP_INDEX":
      return !!f.index_name.trim();
    case "ADD_CONSTRAINT":
      return !!f.constraint_name.trim() && !!f.check_expression.trim();
    case "RENAME_COLUMN":
      return !!f.column.trim() && !!f.new_column_name.trim();
    case "ALTER_COLUMN_TYPE":
      return !!f.column.trim() && !!f.type.trim();
    default: // ADD_COLUMN, DROP_COLUMN, ADD_INDEX, SET_NOT_NULL
      return !!f.column.trim();
  }
}

// needsExistingColumn reports whether an operation's "Column" field must
// name a column that ALREADY exists (so it should be a dropdown fed by
// ListColumns) rather than free text. ADD_COLUMN is the one exception —
// its column is being CREATED, so it can never be picked from a list of
// existing ones. DROP_INDEX/ADD_CONSTRAINT don't show a column field at
// all (unchanged from before this dropdown work).
export function needsExistingColumn(operation: Operation): boolean {
  return operation !== "ADD_COLUMN" && operation !== "DROP_INDEX" && operation !== "ADD_CONSTRAINT";
}

function strategyTone(strategy: string): "petrol" | "amber" | "neutral" {
  if (strategy === "SHADOW_TABLE") return "amber";
  if (strategy === "EXPAND_BACKFILL") return "petrol";
  return "neutral";
}

const PREVIEW_DEBOUNCE_MS = 500;

interface AsyncQuery<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  retry: () => void;
}

// useAsyncFetch is the shared plumbing behind every read-only catalog
// fetch on this screen (schemas, tables, columns, sample rows, and the
// connection-info banner): pass null instead of a fetcher to skip
// fetching entirely (e.g. no table selected yet, so there's nothing to
// list columns of). T is the FULL response shape — string[] for
// schemas/tables, ColumnInfo[] for columns, SampleRowsResult for the
// sample-rows endpoint (a single object, not a list) — this hook doesn't
// assume an array. Deliberately local to this screen rather than
// promoted to a shared hook — nothing else needs this shape yet, and
// premature extraction just adds an extra layer of indirection to read
// through.
function useAsyncFetch<T>(fetcher: (() => Promise<T>) | null, deps: DependencyList): AsyncQuery<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    if (!fetcher) {
      setData(null);
      setError(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    fetcher()
      .then((result) => {
        if (cancelled) return;
        setData(result);
        setError(null);
      })
      .catch((err) => {
        if (cancelled) return;
        setData(null);
        setError(err instanceof ApiError ? err.message : "Could not load this.");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // fetcher is intentionally excluded: it's a fresh closure every
    // render, and the caller-supplied `deps` already captures everything
    // that should actually trigger a refetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, reloadKey]);

  return { data, error, loading, retry: () => setReloadKey((k) => k + 1) };
}

function RetryableError({ message, onRetry, label }: { message: string; onRetry: () => void; label: string }) {
  return (
    <span className="text-xs text-coral-500">
      {message}{" "}
      <button
        type="button"
        className="underline hover:no-underline"
        onClick={onRetry}
        aria-label={`Retry loading ${label}`}
      >
        Retry
      </button>
    </span>
  );
}

const selectClasses =
  "rounded-md border border-ink-200 px-3 py-2 text-sm text-ink-800 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500 disabled:bg-ink-50 disabled:text-ink-400";

export default function NewMigration() {
  const { hasRole } = useAuth();
  const navigate = useNavigate();
  const [form, setForm] = useState<FormState>(initialForm);
  const [preview, setPreview] = useState<PreviewReport | null>(null);
  // Opt-in — see api.estimateWriteLoad's own doc comment for why this
  // isn't just always checked: it blocks for ~10 seconds, so it only
  // runs when explicitly triggered.
  const [writeLoadEstimate, setWriteLoadEstimate] = useState<WriteLoadEstimate | null>(null);
  const [checkingWriteLoad, setCheckingWriteLoad] = useState(false);
  const [writeLoadError, setWriteLoadError] = useState<string | null>(null);

  async function checkWriteLoad() {
    setCheckingWriteLoad(true);
    setWriteLoadError(null);
    try {
      setWriteLoadEstimate(await api.estimateWriteLoad());
    } catch (err) {
      setWriteLoadError(err instanceof ApiError ? err.message : "Could not measure the current write load.");
    } finally {
      setCheckingWriteLoad(false);
    }
  }
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const debounceRef = useRef<number | null>(null);
  // Guards against an in-flight preview request from an earlier keystroke
  // resolving AFTER a newer one and clobbering more current results —
  // requests aren't guaranteed to resolve in the order they were sent.
  const requestIdRef = useRef(0);

  const connectionQuery = useAsyncFetch<ConnectionInfo>(() => api.getConnectionInfo(), []);
  const schemasQuery = useAsyncFetch<string[]>(() => api.listSchemas(), []);
  const tablesQuery = useAsyncFetch<string[]>(form.schema ? () => api.listTables(form.schema) : null, [form.schema]);
  // Columns are fetched as soon as schema+table are both picked,
  // regardless of whether the current operation needs an existing-column
  // dropdown (see needsExistingColumn) — the table-overview panel below
  // needs the full column list unconditionally, and reusing the same
  // fetch avoids a duplicate request when the dropdown IS also shown.
  const columnDropdownNeeded = needsExistingColumn(form.operation);
  const columnsQuery = useAsyncFetch<ColumnInfo[]>(
    form.schema && form.table ? () => api.listColumns(form.schema, form.table) : null,
    [form.schema, form.table],
  );
  const sampleRowsQuery = useAsyncFetch<SampleRowsResult>(
    form.schema && form.table ? () => api.sampleRows(form.schema, form.table) : null,
    [form.schema, form.table],
  );
  // Row count for the table overview panel's header — see api.getTableStats's
  // own doc comment for why this can never disagree with what the
  // migration's own strategy decision is based on.
  const tableStatsQuery = useAsyncFetch<TableStats>(
    form.schema && form.table ? () => api.getTableStats(form.schema, form.table) : null,
    [form.schema, form.table],
  );
  // Fetched once (no dependency array inputs change it) — this is
  // static, compile-time-known domain knowledge, not per-table data.
  // See StrategyMatrix's own doc comment for why the strategy override
  // dropdown needs this at all: forcing an operation through a strategy
  // whose flow has no logic for it used to be silently accepted and
  // then silently did nothing useful (see internal/strategy's
  // validStrategiesByOperation doc comment for the real incident).
  const strategyMatrixQuery = useAsyncFetch<StrategyMatrix>(() => api.getStrategyMatrix(), []);
  // Undefined while strategyMatrixQuery is still loading — every
  // strategy option is shown in that brief window rather than none, so
  // the dropdown doesn't flash empty on first render.
  const validStrategiesForOp = strategyMatrixQuery.data?.[form.operation];

  function handleOperationChange(next: Operation) {
    setForm((f) => {
      const validForNext = strategyMatrixQuery.data?.[next];
      // Always default to "(automatic)" on an operation change — an
      // explicit strategy chosen for the PREVIOUS operation carrying
      // over just because it also happens to be valid for the new one
      // (e.g. picking DIRECT_DDL for ADD_COLUMN, then switching to
      // ALTER_COLUMN_TYPE, which also allows DIRECT_DDL) is a confusing
      // default: the user didn't choose that strategy FOR this
      // operation, it's a leftover from an unrelated previous choice.
      // A fresh operation deserves a fresh, safe default.
      let nextOverride = "";
      if (validForNext && validForNext.length === 1) {
        // Only one strategy is ever valid for this operation — showing
        // "(automatic)" as a separate choice alongside that single
        // strategy would be showing two options that always resolve to
        // exactly the same outcome. Select it explicitly instead, so
        // what's displayed matches what's actually submitted (see the
        // render logic just below, which skips "(automatic)" in this
        // same case).
        nextOverride = validForNext[0];
      }
      return { ...f, operation: next, strategy_override: nextOverride };
    });
  }

  function update<K extends keyof FormState>(key: K, value: FormState[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }

  // Changing schema/table invalidates whatever was picked below it in the
  // hierarchy (a table from the old schema, a column from the old table)
  // — clearing them here, right where the change happens, keeps the
  // dependency obvious rather than burying it in an effect.
  function handleSchemaChange(value: string) {
    setForm((f) => ({ ...f, schema: value, table: "", column: "" }));
  }
  function handleTableChange(value: string) {
    setForm((f) => ({ ...f, table: value, column: "" }));
  }
  // For ALTER_COLUMN_TYPE specifically, pre-fill Type with the selected
  // column's CURRENT type — a starting point to tweak (e.g. widen a
  // varchar) rather than typing a type out from scratch. Never
  // overwrites something the operator already typed.
  function handleColumnChange(value: string) {
    setForm((f) => {
      const next = { ...f, column: value };
      if (f.operation === "ALTER_COLUMN_TYPE" && !f.type) {
        const info = columnsQuery.data?.find((c) => c.Name === value);
        if (info) next.type = info.Type;
      }
      return next;
    });
  }

  useEffect(() => {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);

    if (!isReadyForPreview(form)) {
      setPreview(null);
      setPreviewError(null);
      setPreviewLoading(false);
      return;
    }

    const thisRequestId = ++requestIdRef.current;
    setPreviewLoading(true);

    debounceRef.current = window.setTimeout(async () => {
      try {
        const result = await api.previewMigration(toRequest(form));
        if (thisRequestId === requestIdRef.current) {
          setPreview(result);
          setPreviewError(null);
        }
      } catch (err) {
        if (thisRequestId === requestIdRef.current) {
          setPreview(null);
          setPreviewError(err instanceof ApiError ? err.message : "Could not generate a preview.");
        }
      } finally {
        if (thisRequestId === requestIdRef.current) setPreviewLoading(false);
      }
    }, PREVIEW_DEBOUNCE_MS);

    return () => {
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
    };
  }, [form]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitError(null);
    setSubmitting(true);
    try {
      const job = await api.startMigration(toRequest(form));
      navigate(`/migrations/${job.JobID}`);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? err.message : "Could not start the migration.");
    } finally {
      setSubmitting(false);
    }
  }

  if (!hasRole("operator")) {
    return (
      <Card>
        <div className="px-5 py-16 text-center text-sm text-ink-500">
          You need operator access to start migrations.
        </div>
      </Card>
    );
  }

  const ready = isReadyForPreview(form);

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-lg font-medium text-ink-800">New migration</h1>
        <p className="text-sm text-ink-500">The exact SQL and any risks are shown live as you fill this in.</p>
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2 lg:items-start">
        <Card>
          <CardBody>
            <form onSubmit={handleSubmit} className="flex flex-col gap-4">
              {/* Read-only — deliberately not an editable connection form.
                  Every migration always targets the single database this
                  server was started with (PGARCHIMIGRATOR_DATABASE_URL); this
                  banner exists purely so the operator can double-check
                  which one that is before submitting. */}
              {connectionQuery.data && (
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md bg-ink-50 px-3 py-2 text-xs text-ink-500">
                  <span className="font-medium text-ink-600">Connected to</span>
                  <span className="font-mono text-ink-700">
                    {connectionQuery.data.Host}:{connectionQuery.data.Port}
                  </span>
                  <span aria-hidden="true">·</span>
                  <span className="font-mono text-ink-700">{connectionQuery.data.Database}</span>
                  <span aria-hidden="true">·</span>
                  <span>
                    as <span className="font-mono text-ink-700">{connectionQuery.data.Username}</span>
                  </span>
                  {connectionQuery.data.PostgresVersion > 0 && (
                    <>
                      <span aria-hidden="true">·</span>
                      <span
                        className="font-mono text-ink-700"
                        title={connectionQuery.data.PostgresVersionString}
                      >
                        PostgreSQL {connectionQuery.data.PostgresVersion}
                      </span>
                      {/* below_minimum can't actually happen here in
                          practice — the server refuses to even start
                          serving requests against an unsupported version
                          (see internal/orchestrator's VersionCheck) — but
                          the badge is still handled defensively rather
                          than assumed impossible, matching this
                          component's general "don't assume, render what
                          the API actually says" approach elsewhere. */}
                      {connectionQuery.data.VersionSupportStatus === "below_minimum" && (
                        <Badge tone="coral">unsupported version</Badge>
                      )}
                      {connectionQuery.data.VersionSupportStatus === "newer_than_tested" && (
                        <Badge tone="amber">newer than tested</Badge>
                      )}
                    </>
                  )}
                </div>
              )}
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <label className="flex flex-col gap-1.5">
                  <span className="text-sm font-medium text-ink-700">Schema</span>
                  <select
                    value={form.schema}
                    onChange={(e) => handleSchemaChange(e.target.value)}
                    disabled={schemasQuery.loading}
                    aria-busy={schemasQuery.loading}
                    required
                    className={selectClasses}
                  >
                    {schemasQuery.data === null ? (
                      <option value={form.schema}>{schemasQuery.loading ? "Loading…" : form.schema || "—"}</option>
                    ) : (
                      schemasQuery.data.map((s) => (
                        <option key={s} value={s}>
                          {s}
                        </option>
                      ))
                    )}
                  </select>
                  {schemasQuery.error && (
                    <RetryableError message={schemasQuery.error} onRetry={schemasQuery.retry} label="schemas" />
                  )}
                </label>

                <label className="flex flex-col gap-1.5">
                  <span className="text-sm font-medium text-ink-700">Table</span>
                  <select
                    value={form.table}
                    onChange={(e) => handleTableChange(e.target.value)}
                    disabled={tablesQuery.loading || !form.schema}
                    aria-busy={tablesQuery.loading}
                    required
                    className={selectClasses}
                  >
                    <option value="" disabled>
                      {tablesQuery.loading
                        ? "Loading…"
                        : tablesQuery.data?.length === 0
                          ? "No tables found"
                          : "Select a table"}
                    </option>
                    {tablesQuery.data?.map((t) => (
                      <option key={t} value={t}>
                        {t}
                      </option>
                    ))}
                  </select>
                  {tablesQuery.error && (
                    <RetryableError message={tablesQuery.error} onRetry={tablesQuery.retry} label="tables" />
                  )}
                </label>
              </div>

              <TextField
                label="Name"
                placeholder="optional — e.g. Q3 billing schema update"
                value={form.name}
                onChange={(e) => update("name", e.target.value)}
              />

              <label className="flex flex-col gap-1.5">
                <span className="text-sm font-medium text-ink-700">Description</span>
                <textarea
                  placeholder="optional — what is this migration for, and why now?"
                  value={form.description}
                  onChange={(e) => update("description", e.target.value)}
                  rows={2}
                  className="resize-y rounded-md border border-ink-200 px-3 py-2 text-sm text-ink-800 placeholder:text-ink-300 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500"
                />
              </label>

              <label className="flex flex-col gap-1.5">
                <span className="text-sm font-medium text-ink-700">Operation</span>
                <select
                  value={form.operation}
                  onChange={(e) => handleOperationChange(e.target.value as Operation)}
                  className={selectClasses}
                >
                  {OPERATIONS.map((op) => (
                    <option key={op} value={op}>
                      {op}
                    </option>
                  ))}
                </select>
              </label>

              {form.operation === "ADD_COLUMN" && (
                <TextField
                  id="field-column"
                  label="Column"
                  required
                  value={form.column}
                  onChange={(e) => update("column", e.target.value)}
                />
              )}

              {columnDropdownNeeded && (
                <label className="flex flex-col gap-1.5">
                  <span className="text-sm font-medium text-ink-700">
                    {form.operation === "RENAME_COLUMN" ? "Current column name" : "Column"}
                  </span>
                  <select
                    value={form.column}
                    onChange={(e) => handleColumnChange(e.target.value)}
                    disabled={columnsQuery.loading || !form.table}
                    aria-busy={columnsQuery.loading}
                    required
                    className={selectClasses}
                  >
                    <option value="" disabled>
                      {columnsQuery.loading
                        ? "Loading…"
                        : columnsQuery.data?.length === 0
                          ? "No columns found"
                          : "Select a column"}
                    </option>
                    {columnsQuery.data?.map((c) => (
                      <option key={c.Name} value={c.Name}>
                        {c.Name} — {c.Type}
                      </option>
                    ))}
                  </select>
                  {columnsQuery.error && (
                    <RetryableError message={columnsQuery.error} onRetry={columnsQuery.retry} label="columns" />
                  )}
                </label>
              )}

              {form.operation === "RENAME_COLUMN" && (
                <TextField
                  label="New column name"
                  required
                  value={form.new_column_name}
                  onChange={(e) => update("new_column_name", e.target.value)}
                />
              )}

              {(form.operation === "ADD_COLUMN" || form.operation === "ALTER_COLUMN_TYPE") && (
                <TextField
                  label="Type"
                  placeholder="e.g. text, integer, varchar(100)"
                  required={form.operation === "ALTER_COLUMN_TYPE"}
                  value={form.type}
                  onChange={(e) => update("type", e.target.value)}
                />
              )}

              {form.operation === "ADD_COLUMN" && (
                <>
                  <TextField
                    label="Default"
                    placeholder="e.g. 'active' or now()"
                    value={form.default}
                    onChange={(e) => update("default", e.target.value)}
                  />
                  <label className="flex items-center gap-2 text-sm text-ink-700">
                    <input
                      type="checkbox"
                      checked={form.volatile_default}
                      onChange={(e) => update("volatile_default", e.target.checked)}
                    />
                    Volatile default (e.g. now(), random()) — triggers Expand &amp; Backfill
                  </label>
                </>
              )}

              {(form.operation === "ADD_INDEX" || form.operation === "DROP_INDEX") && (
                <TextField
                  label="Index name"
                  placeholder={form.operation === "ADD_INDEX" ? "optional, auto-generated if omitted" : "required"}
                  required={form.operation === "DROP_INDEX"}
                  value={form.index_name}
                  onChange={(e) => update("index_name", e.target.value)}
                />
              )}

              {(form.operation === "SET_NOT_NULL" || form.operation === "ADD_CONSTRAINT") && (
                <TextField
                  label="Constraint name"
                  placeholder={form.operation === "SET_NOT_NULL" ? "optional, auto-generated if omitted" : "required"}
                  required={form.operation === "ADD_CONSTRAINT"}
                  value={form.constraint_name}
                  onChange={(e) => update("constraint_name", e.target.value)}
                />
              )}

              {form.operation === "ADD_CONSTRAINT" && (
                <TextField
                  label="Check expression"
                  required
                  placeholder="e.g. price > 0"
                  value={form.check_expression}
                  onChange={(e) => update("check_expression", e.target.value)}
                />
              )}

              <label className="flex flex-col gap-1.5">
                <span className="text-sm font-medium text-ink-700">Strategy override</span>
                <select
                  value={form.strategy_override}
                  onChange={(e) => update("strategy_override", e.target.value)}
                  className={selectClasses}
                >
                  {/* "(automatic)" is only meaningfully different from an
                      explicit choice when more than one strategy is
                      actually possible for this operation (e.g.
                      ADD_COLUMN: DIRECT_DDL vs EXPAND_BACKFILL depending
                      on whether the default is volatile) — when only one
                      strategy is ever valid (see handleOperationChange,
                      which already selects it explicitly in that case),
                      showing both would just be two options that always
                      resolve to the exact same outcome. */}
                  {(!validStrategiesForOp || validStrategiesForOp.length > 1) && (
                    <option value="">(automatic)</option>
                  )}
                  {/* Only strategies this operation's own flow actually
                      knows how to execute — see StrategyMatrix's doc
                      comment for the real incident (ADD_INDEX silently
                      forced through SHADOW_TABLE, which replicated the
                      entire table and swapped it for an unchanged copy
                      without ever creating the requested index) this
                      restriction exists to make unreachable through the
                      UI, not just documented as a footgun to avoid. */}
                  {(validStrategiesForOp ?? ["DIRECT_DDL", "EXPAND_BACKFILL", "SHADOW_TABLE"]).map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>

              {submitError && <p className="text-sm text-coral-500">{submitError}</p>}

              <Button type="submit" disabled={submitting || !ready} className="mt-1">
                {submitting ? "Starting…" : "Start migration"}
              </Button>
            </form>
          </CardBody>
        </Card>

        <div className="lg:sticky lg:top-6 flex flex-col gap-6">
          {form.schema && form.table && (
            <Card>
              <CardHeader className="flex items-center justify-between">
                <span className="font-mono text-sm font-medium text-ink-700">
                  {form.schema}.{form.table}
                </span>
                {columnsQuery.data && (
                  <span className="text-xs text-ink-400">
                    {columnsQuery.data.length} column{columnsQuery.data.length === 1 ? "" : "s"}
                    {tableStatsQuery.data && (
                      <> · {formatRowCount(tableStatsQuery.data.EstimatedRowCount)} rows (est.)</>
                    )}
                  </span>
                )}
              </CardHeader>
              <CardBody className="flex flex-col gap-4">
                {columnsQuery.loading && <p className="text-sm text-ink-400">Loading columns…</p>}
                {columnsQuery.error && (
                  <RetryableError message={columnsQuery.error} onRetry={columnsQuery.retry} label="columns" />
                )}
                {columnsQuery.data && columnsQuery.data.length > 0 && (
                  <div className="overflow-x-auto">
                    <table aria-label="Columns" className="w-full text-xs">
                      <thead>
                        <tr className="border-b border-ink-100 text-left text-ink-400">
                          <th className="py-1 pr-3 font-medium">Column</th>
                          <th className="py-1 pr-3 font-medium">Type</th>
                          <th className="py-1 pr-3 font-medium">Nullable</th>
                          <th className="py-1 font-medium">Default</th>
                        </tr>
                      </thead>
                      <tbody>
                        {columnsQuery.data.map((c) => (
                          <tr key={c.Name} className="border-b border-ink-50 last:border-0">
                            <td className="whitespace-nowrap py-1.5 pr-3">
                              <div className="flex items-center gap-1.5">
                                <span className="font-mono text-ink-800">{c.Name}</span>
                                {c.IsPrimaryKey && <Badge tone="petrol">PK</Badge>}
                              </div>
                            </td>
                            <td className="whitespace-nowrap py-1.5 pr-3 font-mono text-ink-600">{c.Type}</td>
                            <td className="whitespace-nowrap py-1.5 pr-3 text-ink-500">
                              {c.Nullable ? "Yes" : "NOT NULL"}
                            </td>
                            <td className="whitespace-nowrap py-1.5 font-mono text-ink-500">{c.Default || "—"}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}

                <div>
                  <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-ink-400">
                    Sample data (up to 5 rows)
                  </p>
                  {sampleRowsQuery.loading && <p className="text-sm text-ink-400">Loading sample…</p>}
                  {sampleRowsQuery.error && (
                    <RetryableError message={sampleRowsQuery.error} onRetry={sampleRowsQuery.retry} label="sample data" />
                  )}
                  {sampleRowsQuery.data && sampleRowsQuery.data.Rows.length === 0 && (
                    <p className="text-sm text-ink-400">This table is empty.</p>
                  )}
                  {sampleRowsQuery.data && sampleRowsQuery.data.Rows.length > 0 && (
                    <div className="overflow-x-auto">
                      <table aria-label="Sample rows" className="w-full text-xs">
                        <thead>
                          <tr className="border-b border-ink-100 text-left text-ink-400">
                            {sampleRowsQuery.data.Columns.map((col) => (
                              <th key={col} className="whitespace-nowrap py-1 pr-3 font-medium">
                                {col}
                              </th>
                            ))}
                          </tr>
                        </thead>
                        <tbody>
                          {sampleRowsQuery.data.Rows.map((row, i) => (
                            <tr key={i} className="border-b border-ink-50 last:border-0">
                              {row.map((cell, j) => (
                                <td key={j} className="whitespace-nowrap py-1.5 pr-3 font-mono text-ink-600">
                                  {cell}
                                </td>
                              ))}
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}
                </div>
              </CardBody>
            </Card>
          )}

          <Card>
            <CardHeader>
              <span className="text-sm font-medium text-ink-700">Preview</span>
            </CardHeader>
            <CardBody>
              {!ready && <p className="text-sm text-ink-400">Fill in the required fields to see a live preview.</p>}
              {ready && previewLoading && !preview && <p className="text-sm text-ink-400">Generating preview…</p>}
              {previewError && <p className="text-sm text-coral-500">{previewError}</p>}

              {preview && (
                <div className="flex flex-col gap-4">
                  <div className="flex items-center justify-between">
                    <Badge tone={strategyTone(preview.Strategy)}>{preview.Strategy}</Badge>
                    <span className="font-mono text-xs text-ink-400">
                      ~{preview.EstimatedRows.toLocaleString()} row(s)
                    </span>
                  </div>

                  {preview.Statements.length > 0 && (
                    <div>
                      <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-ink-400">SQL</p>
                      <div className="flex flex-col gap-1.5">
                        {preview.Statements.map((s, i) => (
                          <pre
                            key={i}
                            className="overflow-x-auto rounded-md bg-ink-900 px-3 py-2 font-mono text-xs text-ink-50"
                          >
                            {s}
                          </pre>
                        ))}
                      </div>
                    </div>
                  )}

                  {preview.Warnings.length > 0 && (
                    <div>
                      <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-coral-500">Warnings</p>
                      <ul className="flex flex-col gap-1.5">
                        {preview.Warnings.map((w, i) => (
                          <li key={i} className="rounded-md bg-coral-50 px-3 py-2 text-sm text-coral-600">
                            {w}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}

                  {preview.Notes.length > 0 && (
                    <div>
                      <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-ink-400">Notes</p>
                      <ul className="flex flex-col gap-1.5">
                        {preview.Notes.map((n, i) => (
                          <li key={i} className="text-sm text-ink-600">
                            {n}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}

                  {/* Only offered for SHADOW_TABLE — this is where it
                      matters most (see db.SampleWALGenerationRate's own
                      doc comment for the real incident: a SHADOW_TABLE
                      migration's delta sync can, under heavy sustained
                      write load, never converge). Opt-in because the
                      check itself blocks for ~10 seconds — never run
                      automatically just because SHADOW_TABLE was
                      selected. */}
                  {preview.Strategy === "SHADOW_TABLE" && (
                    <div className="border-t border-ink-100 pt-4">
                      <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-ink-400">
                        Current write load
                      </p>
                      {!writeLoadEstimate && !checkingWriteLoad && (
                        <Button type="button" variant="ghost" onClick={checkWriteLoad}>
                          Check current write load (~10s)
                        </Button>
                      )}
                      {checkingWriteLoad && (
                        <p className="text-sm text-ink-400" aria-live="polite">
                          Measuring for 10 seconds…
                        </p>
                      )}
                      {writeLoadError && <p className="text-sm text-coral-500">{writeLoadError}</p>}
                      {writeLoadEstimate && (
                        <div className="flex flex-col gap-1.5">
                          <p className="font-mono text-sm text-ink-700">
                            {formatBytes(writeLoadEstimate.bytesPerSecond)}/s across the whole database (sampled over{" "}
                            {writeLoadEstimate.sampleSeconds}s — not specific to this table)
                          </p>
                          {writeLoadEstimate.caution && (
                            <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-700">
                              This is a genuinely busy database right now. Under sustained write load like this, a
                              SHADOW_TABLE migration's replication catch-up can, in rare cases, never fully converge
                              — consider running during a quieter period, or watch the replication lag indicator
                              closely once this migration starts.
                            </p>
                          )}
                        </div>
                      )}
                    </div>
                  )}
                </div>
              )}
            </CardBody>
          </Card>
        </div>
      </div>
    </div>
  );
}
