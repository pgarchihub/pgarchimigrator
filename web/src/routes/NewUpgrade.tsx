import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import { buildDsn, emptyConnectionFields, isConnectionFieldsComplete, type ConnectionFields } from "../lib/dsn";
import type { IntrospectedSchema, TableRef } from "../lib/types";
import { Button } from "../ui/Button";
import { Card, CardBody, CardHeader } from "../ui/Card";
import { ConnectionFieldsInput } from "../ui/ConnectionFields";
import { SchemaTablePicker, tableKey } from "../ui/SchemaTablePicker";
import { TextField } from "../ui/TextField";

export default function NewUpgrade() {
  const navigate = useNavigate();
  const [source, setSource] = useState<ConnectionFields>(emptyConnectionFields());
  const [target, setTarget] = useState<ConnectionFields>(emptyConnectionFields());
  const [showReplicationOverride, setShowReplicationOverride] = useState(false);
  const [replicationHost, setReplicationHost] = useState("");
  const [replicationPort, setReplicationPort] = useState("");

  // Scope: EITHER a fetched schema/table selection (introspectedSchemas
  // set, selectedTables drives what's sent) OR a plain typed schema
  // list (schemasRaw) — see handleSubmit's own comment for exactly how
  // the two are mutually exclusive, matching
  // engines/postgresql/upgrade.Job.Tables' own doc comment on the same design
  // decision.
  const [introspectedSchemas, setIntrospectedSchemas] = useState<IntrospectedSchema[] | null>(null);
  const [selectedTables, setSelectedTables] = useState<Set<string>>(new Set());
  const [introspecting, setIntrospecting] = useState(false);
  const [introspectError, setIntrospectError] = useState<string | null>(null);
  const [schemasRaw, setSchemasRaw] = useState("");

  // Target-side introspection is read-only — a verification convenience
  // ("what's already there before I start"), never feeds into the
  // request body the way Source's own selection does.
  const [targetIntrospected, setTargetIntrospected] = useState<IntrospectedSchema[] | null>(null);
  const [targetIntrospecting, setTargetIntrospecting] = useState(false);
  const [targetIntrospectError, setTargetIntrospectError] = useState<string | null>(null);

  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canSubmit = isConnectionFieldsComplete(source) && isConnectionFieldsComplete(target);
  const totalTableCount = introspectedSchemas?.reduce((sum, s) => sum + s.tables.length, 0) ?? 0;

  async function handleIntrospect() {
    setIntrospecting(true);
    setIntrospectError(null);
    try {
      const result = await api.introspectUpgradeSource(buildDsn(source));
      setIntrospectedSchemas(result.schemas);
      // Default to everything selected — matches this screen's own
      // prior default behavior (an empty schema list meant "include
      // everything"); the picker lets the user narrow down from there
      // by unchecking what they don't want.
      const all = new Set<string>();
      for (const schema of result.schemas) {
        for (const table of schema.tables) {
          all.add(tableKey(schema.name, table));
        }
      }
      setSelectedTables(all);
    } catch (err) {
      setIntrospectError(err instanceof ApiError ? err.message : "Could not fetch schemas and tables.");
    } finally {
      setIntrospecting(false);
    }
  }

  // handleIntrospectTarget reuses the exact same introspection endpoint
  // as Source's own fetch — it has no idea (and doesn't need to) which
  // side of an upgrade is asking; connecting and listing schemas/tables
  // is identical either way. Purely informational here: nothing this
  // returns is ever submitted with the request.
  async function handleIntrospectTarget() {
    setTargetIntrospecting(true);
    setTargetIntrospectError(null);
    try {
      const result = await api.introspectUpgradeSource(buildDsn(target));
      setTargetIntrospected(result.schemas);
    } catch (err) {
      setTargetIntrospectError(err instanceof ApiError ? err.message : "Could not check the target instance.");
    } finally {
      setTargetIntrospecting(false);
    }
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      // The replication override reuses source's own username/password/
      // database — see ConnectionFieldsInput's own doc comment and this
      // screen's "Advanced" note below: in practice, only the HOST
      // (occasionally the port) ever differs between "how this app
      // reaches source" and "how the target's own server reaches
      // source" — everything else (credentials, database name) stays
      // the same.
      const sourceReplicationDsn = replicationHost.trim()
        ? buildDsn({ ...source, host: replicationHost.trim(), port: replicationPort.trim() || source.port })
        : undefined;

      // Mutually exclusive with schemas, matching
      // engines/postgresql/upgrade.Job.Tables' own doc comment: once schemas have
      // been fetched, the checkbox selection is ALWAYS what's sent —
      // even if every box happens to be checked, this still goes as an
      // explicit table list rather than falling back to schemas, so
      // "fetch, then submit without touching anything" behaves exactly
      // as the visible checkboxes promise.
      let tables: TableRef[] | undefined;
      let schemas: string[] | undefined;
      if (introspectedSchemas) {
        tables = [...selectedTables].map((key) => {
          const [schema, table] = key.split(/\.(.*)/s);
          return { schema, table };
        });
      } else {
        const parsed = schemasRaw
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean);
        schemas = parsed.length > 0 ? parsed : undefined;
      }

      const result = await api.startUpgrade({
        sourceDsn: buildDsn(source),
        targetDsn: buildDsn(target),
        schemas,
        tables,
        sourceReplicationDsn,
      });
      // 202 Accepted — the upgrade itself runs in the background (see
      // api.startUpgrade's own doc comment); navigate straight to the
      // new job's detail page, which polls for live progress from here.
      navigate(`/upgrades/${result.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not start the upgrade.");
      setSubmitting(false);
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-lg font-medium text-ink-800">Start a database migration</h1>
        <p className="text-sm text-ink-500">
          Introspects the source, recreates every in-scope table on the target, syncs via
          PostgreSQL's own logical replication, and validates row-for-row. This tool does not
          perform cutover — repointing application traffic at the new instance is a separate,
          deliberate step you take afterward. Works equally well for moving to a new PostgreSQL
          version, a different host, or a different cloud provider — the source and target don't
          need to be different versions.
        </p>
      </div>

      <Card>
        <CardHeader>Source — the instance data is read FROM</CardHeader>
        <CardBody>
          <ConnectionFieldsInput idPrefix="source" value={source} onChange={setSource} />
        </CardBody>
      </Card>

      <Card>
        <CardHeader>Scope</CardHeader>
        <CardBody>
          <div className="flex flex-col gap-4">
            <div className="flex items-center justify-between">
              <div>
                <p className="text-sm font-medium text-ink-700">Choose schemas and tables</p>
                <p className="text-xs text-ink-400">
                  Connects to the source above and lists what's actually there — no need to know
                  table names by heart.
                </p>
              </div>
              <Button
                type="button"
                variant="secondary"
                onClick={handleIntrospect}
                disabled={!isConnectionFieldsComplete(source) || introspecting}
              >
                {introspecting ? "Fetching…" : introspectedSchemas ? "Refresh" : "Fetch schemas & tables"}
              </Button>
            </div>

            {!isConnectionFieldsComplete(source) && (
              <p className="text-xs text-ink-400">Fill in the Source connection details above first.</p>
            )}

            {introspectError && <p className="text-sm text-coral-600">{introspectError}</p>}

            {introspectedSchemas && (
              <>
                <p className="text-xs text-ink-500">
                  {selectedTables.size} of {totalTableCount} tables selected.
                </p>
                <div className="max-h-96 overflow-y-auto">
                  <SchemaTablePicker
                    schemas={introspectedSchemas}
                    selected={selectedTables}
                    onChange={setSelectedTables}
                  />
                </div>
              </>
            )}

            {!introspectedSchemas && (
              <>
                <TextField
                  id="field-schemas"
                  label="Or type schema names manually (optional)"
                  placeholder="public, billing"
                  value={schemasRaw}
                  onChange={(e) => setSchemasRaw(e.target.value)}
                />
                <p className="-mt-2 text-xs text-ink-400">
                  Comma-separated. Leave empty to include every schema on the source instance.
                </p>
              </>
            )}
          </div>
        </CardBody>
      </Card>

      <Card>
        <CardHeader>Target — the instance data is written TO</CardHeader>
        <CardBody>
          <ConnectionFieldsInput idPrefix="target" value={target} onChange={setTarget} />
          <p className="mt-2 text-xs text-ink-400">
            Must already exist and be reachable — this tool does not provision a new instance for
            you.
          </p>

          <div className="mt-4 border-t border-ink-100 pt-4">
            <div className="flex items-center justify-between">
              <p className="text-xs text-ink-500">
                Optional check: see what already exists on the target before starting.
              </p>
              <Button
                type="button"
                variant="secondary"
                onClick={handleIntrospectTarget}
                disabled={!isConnectionFieldsComplete(target) || targetIntrospecting}
              >
                {targetIntrospecting ? "Checking…" : "View existing schemas & tables"}
              </Button>
            </div>
            {targetIntrospectError && <p className="mt-2 text-sm text-coral-600">{targetIntrospectError}</p>}
            {targetIntrospected && (
              <div className="mt-3 max-h-60 overflow-y-auto rounded-lg border border-ink-100 p-3 text-sm">
                {targetIntrospected.length === 0 && (
                  <p className="text-ink-400">No schemas found — the target looks empty.</p>
                )}
                {targetIntrospected.map((schema) => (
                  <div key={schema.name} className="mb-2 last:mb-0">
                    <p className="font-medium text-ink-700">{schema.name}</p>
                    {schema.tables.length === 0 ? (
                      <p className="ml-3 text-xs text-ink-400">(no tables)</p>
                    ) : (
                      <ul className="ml-3 list-disc text-xs text-ink-500">
                        {schema.tables.map((t) => (
                          <li key={t}>{t}</li>
                        ))}
                      </ul>
                    )}
                  </div>
                ))}
              </div>
            )}
          </div>
        </CardBody>
      </Card>

      <Card>
        <CardBody>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="border-t-0 pt-0">
              <button
                type="button"
                onClick={() => setShowReplicationOverride((v) => !v)}
                className="text-xs font-medium text-petrol-700 hover:text-petrol-800"
              >
                {showReplicationOverride ? "− Hide advanced option" : "+ Advanced: different replication address?"}
              </button>
              {showReplicationOverride && (
                <div className="mt-3 grid grid-cols-2 gap-3">
                  <div className="col-span-2 sm:col-span-1">
                    <TextField
                      id="field-replication-host"
                      label="Replication host"
                      placeholder="pg-logical"
                      value={replicationHost}
                      onChange={(e) => setReplicationHost(e.target.value)}
                    />
                  </div>
                  <div className="col-span-2 sm:col-span-1">
                    <TextField
                      id="field-replication-port"
                      label="Replication port (optional)"
                      placeholder={source.port || "5432"}
                      inputMode="numeric"
                      value={replicationPort}
                      onChange={(e) => setReplicationPort(e.target.value)}
                    />
                  </div>
                  <p className="col-span-2 text-xs text-ink-400">
                    Only needed if the TARGET instance's own PostgreSQL server can't reach the
                    source using the Source host above — for example, in a local Docker Compose
                    setup, this app might reach the source via a host-mapped port (
                    <code className="font-mono">localhost:55432</code>) while the target container
                    needs the Compose network's own hostname (
                    <code className="font-mono">pg-logical:5432</code>). Reuses the Source
                    username/password/database entered above — leave this blank entirely if the
                    Source host is already reachable from the target.
                  </p>
                </div>
              )}
            </div>

            {error && <p className="text-sm text-coral-600">{error}</p>}

            <div className="flex justify-end gap-2 pt-2">
              <Button type="submit" disabled={submitting || !canSubmit}>
                {submitting ? "Starting…" : "Start database migration"}
              </Button>
            </div>
          </form>
        </CardBody>
      </Card>
    </div>
  );
}
