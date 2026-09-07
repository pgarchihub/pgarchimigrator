import { type DependencyList, type FormEvent, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Database as DatabaseIcon } from "lucide-react";
import { api, ApiError } from "../lib/api";
import type { ColumnInfo, ConnectionInfo, Operation, PreviewReport, SampleRowsResult, StartMigrationRequest, StrategyMatrix, TableStats, WriteLoadEstimate } from "../lib/types";
import { useAuth } from "../lib/auth";
import { formatBytes, formatRowCount } from "../lib/format";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { Badge } from "../ui/Badge";
import { TextField } from "../ui/TextField";

// OPERATION_GROUPS drives the Operation dropdown's own <optgroup>
// structure — grouped by what the operation actually acts on (the
// table itself, one of its columns, or one of its indexes), so
// someone scanning 12 operations for "the one that touches indexes"
// doesn't have to read every label. Each group gets its own shade of
// petrol (the project's own single brand color) rather than reaching
// for amber/coral — see tailwind.config.js's own comments: amber is
// reserved strictly for "active/in progress" states and coral strictly
// for destructive warnings, and reusing either here just for visual
// grouping would blur a distinction those colors exist to protect
// elsewhere in the app.
const OPERATION_GROUPS: { label: string; colorClass: string; operations: Operation[] }[] = [
  { label: "Table", colorClass: "text-petrol-800", operations: ["RENAME_TABLE", "PARTITION_TABLE"] },
  {
    label: "Column",
    colorClass: "text-petrol-600",
    operations: [
      "ADD_COLUMN",
      "DROP_COLUMN",
      "ALTER_COLUMN_TYPE",
      "SET_NOT_NULL",
      "RENAME_COLUMN",
      "ADD_GENERATED_COLUMN",
      "ADD_FOREIGN_KEY",
      "ADD_CONSTRAINT",
    ],
  },
  { label: "Index", colorClass: "text-petrol-400", operations: ["ADD_INDEX", "DROP_INDEX"] },
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
  new_table_name: string;
  referenced_table: string;
  referenced_column: string;
  on_delete: string;
  generated_expression: string;
  partition_column: string;
  partition_strategy: string;
  partition_include_default: boolean;
  // Explicit bounds, entered as raw JSON text — matches the CLI's own
  // --partition-bounds flag design (rather than a fully dynamic
  // multi-row bounds editor), so the same mental model works whether
  // someone reaches for the web UI or the CLI. Parsed just before
  // submission — see toRequest below.
  partition_bounds_raw: string;
  // The RANGE-only rule-based shortcut (see strategy.ExpandPartitionRule)
  // — an alternative to partition_bounds_raw, not required alongside it.
  partition_interval: string;
  partition_rule_from: string;
  partition_rule_to: string;
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
  new_table_name: "",
  referenced_table: "",
  referenced_column: "",
  on_delete: "",
  generated_expression: "",
  partition_column: "",
  partition_strategy: "",
  partition_include_default: false,
  partition_bounds_raw: "",
  partition_interval: "",
  partition_rule_from: "",
  partition_rule_to: "",
  name: "",
  description: "",
};

function toRequest(f: FormState): StartMigrationRequest {
  let partitionBounds: StartMigrationRequest["partition_bounds"];
  if (f.operation === "PARTITION_TABLE" && f.partition_bounds_raw.trim()) {
    try {
      partitionBounds = JSON.parse(f.partition_bounds_raw);
    } catch {
      // Left undefined on a parse failure — the request will then fail
      // server-side with a clear "invalid partition bounds" error
      // rather than silently dropping what the user typed. The preview
      // panel's own error display (see the request-error handling
      // further down this file) surfaces that message directly.
    }
  }
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
    new_table_name: f.new_table_name || undefined,
    referenced_table: f.referenced_table || undefined,
    referenced_column: f.referenced_column || undefined,
    on_delete: f.on_delete || undefined,
    generated_expression: f.generated_expression || undefined,
    partition_column: f.partition_column || undefined,
    partition_strategy: f.partition_strategy || undefined,
    partition_include_default: f.partition_include_default,
    partition_bounds: partitionBounds,
    partition_interval: f.partition_interval || undefined,
    partition_rule_from: f.partition_rule_from || undefined,
    partition_rule_to: f.partition_rule_to || undefined,
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
    case "RENAME_TABLE":
      // No column involved at all — this operation acts on the table
      // itself, matching internal/strategy.ColumnChange.NewTableName's
      // own doc comment.
      return !!f.new_table_name.trim();
    case "ADD_FOREIGN_KEY":
      return (
        !!f.column.trim() &&
        !!f.constraint_name.trim() &&
        !!f.referenced_table.trim() &&
        !!f.referenced_column.trim()
      );
    case "ADD_GENERATED_COLUMN":
      return !!f.column.trim() && !!f.type.trim() && !!f.generated_expression.trim();
    case "PARTITION_TABLE": {
      // Also no column field — same reasoning as RENAME_TABLE. Bounds
      // can come from EITHER the raw JSON textarea OR the RANGE-only
      // rule shortcut (not both required) — see toRequest's own
      // handling of partition_bounds_raw for how the textarea gets
      // parsed at submission time.
      if (!f.partition_column.trim() || !f.partition_strategy.trim()) return false;
      const hasExplicitBounds = !!f.partition_bounds_raw.trim();
      const hasRule = !!f.partition_interval.trim() && !!f.partition_rule_from.trim() && !!f.partition_rule_to.trim();
      return hasExplicitBounds || hasRule;
    }
    default: // ADD_COLUMN, DROP_COLUMN, ADD_INDEX, SET_NOT_NULL
      return !!f.column.trim();
  }
}

// needsExistingColumn reports whether an operation's "Column" field must
// name a column that ALREADY exists (so it should be a dropdown fed by
// ListColumns) rather than free text. ADD_COLUMN is the one exception —
// its column is being CREATED, so it can never be picked from a list of
// existing ones. DROP_INDEX/ADD_CONSTRAINT/RENAME_TABLE/PARTITION_TABLE
// don't show a column field at all (unchanged from before this dropdown
// work, now joined by the two operations that don't act on a single
// column at all).
export function needsExistingColumn(operation: Operation): boolean {
  return (
    operation !== "ADD_COLUMN" &&
    operation !== "DROP_INDEX" &&
    operation !== "ADD_CONSTRAINT" &&
    operation !== "RENAME_TABLE" &&
    operation !== "PARTITION_TABLE" &&
    operation !== "ADD_GENERATED_COLUMN"
  );
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
  // ADD_FOREIGN_KEY's own "Referenced column" dropdown needs the
  // REFERENCED table's columns (not the source table's, already covered
  // by columnsQuery above) — fetched only once a referenced table has
  // actually been chosen. Same-schema only for now: strategy.
  // ColumnChange has no ReferencedSchema field yet (only
  // ReferencedTable/ReferencedColumn), so a cross-schema reference isn't
  // something this form can express even if the dropdown offered it —
  // see the Referenced table dropdown's own comment on this.
  const referencedColumnsQuery = useAsyncFetch<ColumnInfo[]>(
    form.schema && form.referenced_table ? () => api.listColumns(form.schema, form.referenced_table) : null,
    [form.schema, form.referenced_table],
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

      {/* Read-only — deliberately not an editable connection form. Every
          migration always targets the single database this server was
          started with (PGARCHIMIGRATOR_DATABASE_URL); this banner exists
          purely so the operator can double-check which one that is
          before submitting. Full-width and placed right under the page
          title (rather than tucked inside the form card below) so it's
          the first thing anyone sees before they start filling anything
          in — a person switching between several pgArchiMigrator
          instances shouldn't have to scroll into the form to confirm
          which database they're about to touch. */}
      {connectionQuery.data && (
        <Card>
          <CardBody className="flex flex-wrap items-center gap-x-8 gap-y-3">
            <DatabaseIcon className="h-8 w-8 shrink-0 text-petrol-600" aria-hidden="true" />
            <div>
              <p className="text-xs font-bold text-ink-700">Hostname/IP</p>
              <p className="font-mono text-sm text-ink-600">{connectionQuery.data.Host}</p>
            </div>
            <div>
              <p className="text-xs font-bold text-ink-700">Port</p>
              <p className="font-mono text-sm text-ink-600">{connectionQuery.data.Port}</p>
            </div>
            <div>
              <p className="text-xs font-bold text-ink-700">Username</p>
              <p className="font-mono text-sm text-ink-600">{connectionQuery.data.Username}</p>
            </div>
            <div>
              <p className="text-xs font-bold text-ink-700">Database Name</p>
              <p className="font-mono text-sm text-ink-600">{connectionQuery.data.Database}</p>
            </div>
            <div>
              <p className="text-xs font-bold text-ink-700">Database Engine/Version</p>
              {connectionQuery.data.PostgresVersion > 0 ? (
                <p className="flex items-center gap-2 font-mono text-sm text-ink-600">
                  <span title={connectionQuery.data.PostgresVersionString}>
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
                </p>
              ) : (
                <p className="text-sm text-ink-400">—</p>
              )}
            </div>
          </CardBody>
        </Card>
      )}

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2 lg:items-start">
        <Card>
          <CardBody>
            <form onSubmit={handleSubmit} className="flex flex-col gap-4">
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
                  {OPERATION_GROUPS.map((group) => (
                    <optgroup key={group.label} label={group.label}>
                      {group.operations.map((op) => (
                        <option key={op} value={op} className={group.colorClass}>
                          {op}
                        </option>
                      ))}
                    </optgroup>
                  ))}
                </select>
              </label>

              {(form.operation === "ADD_COLUMN" || form.operation === "ADD_GENERATED_COLUMN") && (
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
                    {form.operation === "RENAME_COLUMN"
                      ? "Current column name"
                      : form.operation === "ADD_FOREIGN_KEY"
                        ? "Local column"
                        : "Column"}
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

              {(form.operation === "ADD_COLUMN" ||
                form.operation === "ALTER_COLUMN_TYPE" ||
                form.operation === "ADD_GENERATED_COLUMN") && (
                <TextField
                  label="Type"
                  placeholder="e.g. text, integer, varchar(100)"
                  required={form.operation === "ALTER_COLUMN_TYPE" || form.operation === "ADD_GENERATED_COLUMN"}
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

              {form.operation === "RENAME_TABLE" && (
                <TextField
                  label="New table name"
                  required
                  value={form.new_table_name}
                  onChange={(e) => update("new_table_name", e.target.value)}
                />
              )}

              {form.operation === "ADD_FOREIGN_KEY" && (
                <>
                  <TextField
                    label="Constraint name"
                    required
                    value={form.constraint_name}
                    onChange={(e) => update("constraint_name", e.target.value)}
                  />
                  <label className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-700">Referenced table</span>
                    <select
                      value={form.referenced_table}
                      onChange={(e) => {
                        update("referenced_table", e.target.value);
                        // A different referenced table invalidates any
                        // previously chosen referenced column — same
                        // "changing the parent resets the dependent
                        // field" pattern the schema/table selects above
                        // already use.
                        update("referenced_column", "");
                      }}
                      disabled={tablesQuery.loading || !form.schema}
                      aria-busy={tablesQuery.loading}
                      required
                      className={selectClasses}
                    >
                      <option value="">{tablesQuery.loading ? "Loading…" : "(choose a table)"}</option>
                      {/* Same schema only — see referencedColumnsQuery's
                          own comment on why a cross-schema reference
                          isn't something this form can express yet.
                          Excludes the source table itself: not because a
                          self-referencing foreign key is invalid in
                          PostgreSQL (it genuinely isn't — a manager_id
                          column referencing the same table's own id is
                          a completely ordinary pattern), but because
                          this dropdown's own primary job is picking a
                          DIFFERENT, already-existing table to point at,
                          and offering the source table back to itself
                          here invites picking it by mistake. */}
                      {tablesQuery.data
                        ?.filter((t) => t !== form.table)
                        .map((t) => (
                          <option key={t} value={t}>
                            {t}
                          </option>
                        ))}
                    </select>
                  </label>
                  <label className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-700">Referenced column</span>
                    <select
                      value={form.referenced_column}
                      onChange={(e) => update("referenced_column", e.target.value)}
                      disabled={referencedColumnsQuery.loading || !form.referenced_table}
                      aria-busy={referencedColumnsQuery.loading}
                      required
                      className={selectClasses}
                    >
                      <option value="">
                        {referencedColumnsQuery.loading
                          ? "Loading…"
                          : form.referenced_table
                            ? "(choose a column)"
                            : "(choose a table first)"}
                      </option>
                      {/* Every column is a technically legal target for
                          a foreign key in PostgreSQL as long as it has a
                          UNIQUE or PRIMARY KEY constraint — this list
                          intentionally still offers every column rather
                          than pre-filtering to just those (the metadata
                          this dropdown has access to doesn't currently
                          distinguish "has some unique constraint" from
                          "doesn't"), but primary keys — overwhelmingly
                          the common, correct choice — are marked so
                          they're easy to spot at a glance. */}
                      {referencedColumnsQuery.data?.map((c) => (
                        <option key={c.Name} value={c.Name} className={c.IsPrimaryKey ? "text-petrol-700" : undefined}>
                          {c.Name}
                          {c.IsPrimaryKey ? " — primary key" : ""}
                        </option>
                      ))}
                    </select>
                  </label>
                  {referencedColumnsQuery.error && (
                    <RetryableError
                      message={referencedColumnsQuery.error}
                      onRetry={referencedColumnsQuery.retry}
                      label="referenced table's columns"
                    />
                  )}
                  <label className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-700">On delete</span>
                    <select
                      value={form.on_delete}
                      onChange={(e) => update("on_delete", e.target.value)}
                      className={selectClasses}
                    >
                      <option value="">(default — NO ACTION)</option>
                      <option value="CASCADE">CASCADE</option>
                      <option value="SET NULL">SET NULL</option>
                      <option value="SET DEFAULT">SET DEFAULT</option>
                      <option value="RESTRICT">RESTRICT</option>
                      <option value="NO ACTION">NO ACTION</option>
                    </select>
                  </label>
                </>
              )}

              {form.operation === "ADD_GENERATED_COLUMN" && (
                <TextField
                  label="Generated expression"
                  placeholder="e.g. price * quantity"
                  required
                  value={form.generated_expression}
                  onChange={(e) => update("generated_expression", e.target.value)}
                />
              )}

              {form.operation === "PARTITION_TABLE" && (
                <>
                  <TextField
                    label="Partition column"
                    required
                    value={form.partition_column}
                    onChange={(e) => update("partition_column", e.target.value)}
                  />
                  <label className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-700">Partition strategy</span>
                    <select
                      value={form.partition_strategy}
                      onChange={(e) => update("partition_strategy", e.target.value)}
                      className={selectClasses}
                    >
                      <option value="">(choose one)</option>
                      <option value="RANGE">RANGE</option>
                      <option value="LIST">LIST</option>
                    </select>
                  </label>
                  <label className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-700">Partition bounds (JSON)</span>
                    <textarea
                      placeholder={
                        form.partition_strategy === "LIST"
                          ? '[{"name":"orders_eu","values":["DE","FR"]},{"name":"orders_us","values":["US"]}]'
                          : '[{"name":"orders_2024_01","from":"2024-01-01","to":"2024-02-01"}] — or use the interval shortcut below'
                      }
                      value={form.partition_bounds_raw}
                      onChange={(e) => update("partition_bounds_raw", e.target.value)}
                      rows={3}
                      className="resize-y rounded-md border border-ink-200 px-3 py-2 font-mono text-xs text-ink-800 placeholder:text-ink-300 focus:outline-none focus:ring-2 focus:ring-petrol-500 focus:border-petrol-500"
                    />
                    <span className="text-xs text-ink-400">
                      Required for LIST partitioning. For RANGE, an alternative to the interval shortcut below.
                    </span>
                  </label>
                  {form.partition_strategy === "RANGE" && (
                    <div className="flex flex-col gap-1.5 rounded-md border border-ink-100 bg-ink-50 p-3">
                      <span className="text-xs font-medium uppercase tracking-wide text-ink-400">
                        Or generate bounds from a rule (RANGE only)
                      </span>
                      <label className="flex flex-col gap-1.5">
                        <span className="text-sm font-medium text-ink-700">Interval</span>
                        <select
                          value={form.partition_interval}
                          onChange={(e) => update("partition_interval", e.target.value)}
                          className={selectClasses}
                        >
                          <option value="">(none — use the JSON bounds above)</option>
                          <option value="daily">daily</option>
                          <option value="monthly">monthly</option>
                          <option value="yearly">yearly</option>
                        </select>
                      </label>
                      <TextField
                        label="From"
                        placeholder="YYYY-MM-DD"
                        value={form.partition_rule_from}
                        onChange={(e) => update("partition_rule_from", e.target.value)}
                      />
                      <TextField
                        label="To"
                        placeholder="YYYY-MM-DD"
                        value={form.partition_rule_to}
                        onChange={(e) => update("partition_rule_to", e.target.value)}
                      />
                    </div>
                  )}
                  <label className="flex items-center gap-2 text-sm text-ink-700">
                    <input
                      type="checkbox"
                      checked={form.partition_include_default}
                      onChange={(e) => update("partition_include_default", e.target.checked)}
                      className="rounded border-ink-300 text-petrol-600 focus:ring-petrol-500"
                    />
                    Add a DEFAULT partition (recommended unless your bounds are certainly exhaustive)
                  </label>
                </>
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
