// ConnectionFields is the structured, human-friendly counterpart to a
// raw PostgreSQL connection string — Host/Port/Username/Password/
// Database/SSL mode, the fields most people expect from ANY database
// client, rather than a single opaque string. buildDsn assembles these
// into the same connection-string format the backend API has always
// accepted (sourceDsn/targetDsn/sourceReplicationDsn) — this is a
// purely frontend convenience; nothing on the server changes, and a
// user who prefers pasting a raw string can still do so (see
// NewUpgrade.tsx's own "paste a full connection string instead"
// toggle).
export interface ConnectionFields {
  host: string;
  port: string;
  username: string;
  password: string;
  database: string;
  sslMode: SslMode;
}

// SslMode covers the three modes people actually need day to day —
// libpq itself supports more (verify-ca, verify-full, allow, etc.),
// but those need certificate configuration this simple form has no
// place for; a user needing them can still switch to pasting a raw
// connection string. "prefer" is PostgreSQL's own client default
// (attempts SSL, falls back to plaintext if the server doesn't offer
// it) — safe for both a local, SSL-less Docker Compose instance and a
// TLS-terminated managed one, so it's this form's own default too.
export type SslMode = "prefer" | "require" | "disable";

export function emptyConnectionFields(): ConnectionFields {
  return { host: "", port: "5432", username: "", password: "", database: "", sslMode: "prefer" };
}

// buildDsn assembles a libpq-style connection URI from structured
// fields — the exact string shape sourceDsn/targetDsn/
// sourceReplicationDsn have always expected server-side, so nothing
// downstream needs to know this form exists at all.
export function buildDsn(fields: ConnectionFields): string {
  const user = encodeURIComponent(fields.username);
  const pass = encodeURIComponent(fields.password);
  const db = encodeURIComponent(fields.database);
  // IPv6 literals need bracket syntax in a URI ("[::1]") — a plain
  // hostname or IPv4 address is used as-is. A colon in the host with
  // no brackets already present is the one reasonably reliable signal
  // this is IPv6 rather than, say, a hostname that happens to contain
  // one (hostnames can't contain colons at all, so this check has no
  // real false-positive case to worry about).
  const host = fields.host.includes(":") && !fields.host.startsWith("[") ? `[${fields.host}]` : fields.host;
  const port = fields.port.trim() ? `:${fields.port.trim()}` : "";
  return `postgresql://${user}:${pass}@${host}${port}/${db}?sslmode=${fields.sslMode}`;
}

// isConnectionFieldsComplete reports whether every field a real
// connection needs (password is deliberately allowed to be empty —
// some setups genuinely use passwordless local trust auth) has a
// value — used to gate form submission without duplicating this check
// at every call site.
export function isConnectionFieldsComplete(fields: ConnectionFields): boolean {
  return fields.host.trim() !== "" && fields.username.trim() !== "" && fields.database.trim() !== "";
}

// ParsedDsnDisplay is everything about a connection string that's safe
// to show back to the user — deliberately excludes the password. See
// parseDsnForDisplay's own doc comment for why this matters.
export interface ParsedDsnDisplay {
  host: string;
  port: string;
  username: string;
  database: string;
}

// parseDsnForDisplay extracts host/port/username/database from a
// connection string for READ-ONLY display purposes — e.g. showing
// "what connection info is this retry about to reuse" (see
// UpgradeDetail.tsx's own retry-review panel) without ever putting the
// stored password back on the wire to the browser. The password is
// deliberately NEVER extracted or returned here, even though `new
// URL()` could trivially read it — a stored connection string already
// went from server to a form once (at creation time, typed by hand);
// re-displaying its password on every later page view/retry review
// would be a real, avoidable exposure with no corresponding benefit
// (the user already knows their own password; they don't need it
// echoed back to confirm a retry).
//
// Returns null for a DSN that doesn't parse as a URL — a defensive
// fallback rather than throwing, so a genuinely malformed or
// unexpected stored value degrades to "can't preview this," not a
// crashed page.
export function parseDsnForDisplay(dsn: string): ParsedDsnDisplay | null {
  try {
    const url = new URL(dsn);
    return {
      host: url.hostname,
      port: url.port,
      username: decodeURIComponent(url.username),
      database: decodeURIComponent(url.pathname.replace(/^\//, "")),
    };
  } catch {
    return null;
  }
}
