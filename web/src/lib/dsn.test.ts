import { describe, expect, it } from "vitest";
import { buildDsn, emptyConnectionFields, isConnectionFieldsComplete, parseDsnForDisplay } from "./dsn";

describe("buildDsn", () => {
  it("assembles a standard connection string", () => {
    const dsn = buildDsn({
      host: "db.example.com",
      port: "5432",
      username: "app_user",
      password: "hunter2",
      database: "mydb",
      sslMode: "require",
    });
    expect(dsn).toBe("postgresql://app_user:hunter2@db.example.com:5432/mydb?sslmode=require");
  });

  // Direct regression test for special characters in a password — a
  // real, common case (generated passwords routinely contain @, :, /,
  // #, etc.), which would otherwise corrupt the connection string's
  // own syntax if not percent-encoded.
  it("percent-encodes special characters in username/password/database", () => {
    const dsn = buildDsn({
      host: "localhost",
      port: "5432",
      username: "user@corp",
      password: "p@ss:w/rd#1",
      database: "my db",
      sslMode: "prefer",
    });
    expect(dsn).toContain(encodeURIComponent("user@corp"));
    expect(dsn).toContain(encodeURIComponent("p@ss:w/rd#1"));
    expect(dsn).toContain(encodeURIComponent("my db"));
    // The unencoded special characters must NOT appear literally in
    // the assembled string — that's exactly the corruption this
    // encoding prevents.
    expect(dsn).not.toContain("p@ss:w/rd#1");
  });

  it("wraps an IPv6 host in brackets", () => {
    const dsn = buildDsn({ ...emptyConnectionFields(), host: "::1", username: "u", database: "d" });
    expect(dsn).toContain("[::1]");
  });

  it("does not double-wrap an already-bracketed IPv6 host", () => {
    const dsn = buildDsn({ ...emptyConnectionFields(), host: "[::1]", username: "u", database: "d" });
    expect(dsn).toContain("[::1]");
    expect(dsn).not.toContain("[[::1]]");
  });

  it("omits the port segment entirely when port is empty", () => {
    const dsn = buildDsn({ ...emptyConnectionFields(), host: "localhost", port: "", username: "u", database: "d" });
    expect(dsn).toBe("postgresql://u:@localhost/d?sslmode=prefer");
  });

  it("allows an empty password (passwordless/trust auth)", () => {
    const dsn = buildDsn({ ...emptyConnectionFields(), host: "localhost", username: "u", password: "", database: "d" });
    expect(dsn).toBe("postgresql://u:@localhost:5432/d?sslmode=prefer");
  });
});

describe("isConnectionFieldsComplete", () => {
  it("is false for the empty default", () => {
    expect(isConnectionFieldsComplete(emptyConnectionFields())).toBe(false);
  });

  it("is true once host/username/database are all filled in", () => {
    expect(
      isConnectionFieldsComplete({ ...emptyConnectionFields(), host: "h", username: "u", database: "d" }),
    ).toBe(true);
  });

  // Direct regression test for password being genuinely optional — a
  // passwordless local trust-auth setup is a real, valid configuration,
  // not an incomplete form.
  it("is true even with an empty password", () => {
    expect(
      isConnectionFieldsComplete({ ...emptyConnectionFields(), host: "h", username: "u", database: "d", password: "" }),
    ).toBe(true);
  });

  it("is false when only some required fields are filled in", () => {
    expect(isConnectionFieldsComplete({ ...emptyConnectionFields(), host: "h" })).toBe(false);
  });
});

describe("parseDsnForDisplay", () => {
  it("extracts host/port/username/database", () => {
    const parsed = parseDsnForDisplay("postgresql://app_user:hunter2@db.example.com:5432/mydb?sslmode=require");
    expect(parsed).toEqual({ host: "db.example.com", port: "5432", username: "app_user", database: "mydb" });
  });

  // THE critical regression test for this whole function's reason to
  // exist — the password must never appear anywhere in the returned
  // value, even though it's trivially present in the input string.
  it("never includes the password anywhere in its result", () => {
    const parsed = parseDsnForDisplay("postgresql://app_user:supersecretpassword@host/db");
    expect(JSON.stringify(parsed)).not.toContain("supersecretpassword");
  });

  it("decodes a percent-encoded username and database", () => {
    const parsed = parseDsnForDisplay(`postgresql://${encodeURIComponent("user@corp")}:pw@host/${encodeURIComponent("my db")}`);
    expect(parsed?.username).toBe("user@corp");
    expect(parsed?.database).toBe("my db");
  });

  it("returns null for a string that isn't a valid URL", () => {
    expect(parseDsnForDisplay("not a connection string")).toBeNull();
  });

  it("returns an empty string for a missing port rather than throwing", () => {
    const parsed = parseDsnForDisplay("postgresql://u:p@host/db");
    expect(parsed?.port).toBe("");
  });
});
