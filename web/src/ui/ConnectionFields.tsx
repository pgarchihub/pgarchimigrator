import type { ConnectionFields, SslMode } from "../lib/dsn";
import { TextField } from "./TextField";

// ConnectionFieldsInput renders the standard Host/Port/Username/
// Password/Database/SSL-mode fields most database clients offer —
// see lib/dsn.ts's own doc comment for why this exists instead of a
// single raw connection-string field: it matches what people are
// already used to typing, without requiring familiarity with libpq's
// own URI syntax.
export function ConnectionFieldsInput({
  idPrefix,
  value,
  onChange,
}: {
  idPrefix: string;
  value: ConnectionFields;
  onChange: (next: ConnectionFields) => void;
}) {
  function set<K extends keyof ConnectionFields>(key: K, fieldValue: ConnectionFields[K]) {
    onChange({ ...value, [key]: fieldValue });
  }

  return (
    <div className="grid grid-cols-2 gap-3">
      <div className="col-span-2 sm:col-span-1">
        <TextField
          id={`${idPrefix}-host`}
          label="Host"
          placeholder="db.example.com"
          required
          value={value.host}
          onChange={(e) => set("host", e.target.value)}
        />
      </div>
      <div className="col-span-2 sm:col-span-1">
        <TextField
          id={`${idPrefix}-port`}
          label="Port"
          placeholder="5432"
          inputMode="numeric"
          value={value.port}
          onChange={(e) => set("port", e.target.value)}
        />
      </div>
      <div className="col-span-2 sm:col-span-1">
        <TextField
          id={`${idPrefix}-username`}
          label="Username"
          required
          value={value.username}
          onChange={(e) => set("username", e.target.value)}
        />
      </div>
      <div className="col-span-2 sm:col-span-1">
        <TextField
          id={`${idPrefix}-password`}
          label="Password"
          type="password"
          autoComplete="off"
          value={value.password}
          onChange={(e) => set("password", e.target.value)}
        />
      </div>
      <div className="col-span-2 sm:col-span-1">
        <TextField
          id={`${idPrefix}-database`}
          label="Database"
          required
          value={value.database}
          onChange={(e) => set("database", e.target.value)}
        />
      </div>
      <div className="col-span-2 sm:col-span-1">
        <label htmlFor={`${idPrefix}-sslmode`} className="mb-1 block text-sm font-medium text-ink-700">
          SSL mode
        </label>
        <select
          id={`${idPrefix}-sslmode`}
          value={value.sslMode}
          onChange={(e) => set("sslMode", e.target.value as SslMode)}
          className="w-full rounded-lg border border-ink-200 bg-white px-3 py-2 text-sm text-ink-800 focus:border-petrol-500 focus:outline-none focus:ring-1 focus:ring-petrol-500"
        >
          <option value="prefer">Prefer (default — works with or without TLS)</option>
          <option value="require">Require (managed/cloud databases usually need this)</option>
          <option value="disable">Disable (local/Docker instances without TLS configured)</option>
        </select>
      </div>
    </div>
  );
}
