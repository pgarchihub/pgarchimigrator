import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

vi.mock("../lib/api", () => {
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
      listSchemas: vi.fn(),
      listTables: vi.fn(),
      listColumns: vi.fn(),
      sampleRows: vi.fn(),
      getConnectionInfo: vi.fn(),
      getTableStats: vi.fn(),
      getStrategyMatrix: vi.fn(),
      estimateWriteLoad: vi.fn(),
      previewMigration: vi.fn(),
      startMigration: vi.fn(),
      setupRequired: vi.fn().mockResolvedValue({ required: false }),
      getVersion: vi.fn().mockResolvedValue({ version: "test" }),
    },
    ApiError: MockApiError,
    setUnauthorizedHandler: vi.fn(),
  };
});

import { api, ApiError } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import NewMigration from "./NewMigration";

function renderScreen() {
  return render(
    <AuthProvider>
      <MemoryRouter>
        <NewMigration />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("NewMigration catalog dropdowns", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset();
    vi.mocked(api.listTables).mockReset();
    // Default to empty — columnsQuery now fires automatically as soon as
    // a table is selected (see NewMigration.tsx: it feeds the
    // table-overview panel unconditionally, not just the Column
    // dropdown), so every test that selects a table needs SOME resolved
    // value here even if it doesn't care about column data itself.
    // Tests that DO care override this individually.
    vi.mocked(api.listColumns).mockReset().mockResolvedValue([]);
    // Default to "no data" for both — individual tests override where the
    // banner/table-overview content itself is what's being tested; every
    // other test just needs these to resolve without throwing.
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "localhost", Port: 5432, Username: "pgarchimigrator", Database: "pgarchimigrator_test", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    // Every strategy allowed for every operation by default — individual
    // tests that specifically exercise the restriction override this.
    vi.mocked(api.getStrategyMatrix)
      .mockReset()
      .mockResolvedValue({
        ADD_COLUMN: ["DIRECT_DDL", "EXPAND_BACKFILL"],
        DROP_COLUMN: ["DIRECT_DDL"],
        ALTER_COLUMN_TYPE: ["DIRECT_DDL", "SHADOW_TABLE"],
        ADD_INDEX: ["DIRECT_DDL"],
        DROP_INDEX: ["DIRECT_DDL"],
        SET_NOT_NULL: ["DIRECT_DDL"],
        ADD_CONSTRAINT: ["DIRECT_DDL"],
        RENAME_COLUMN: ["EXPAND_BACKFILL"],
      });
    vi.mocked(api.sampleRows).mockReset().mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.previewMigration).mockReset().mockResolvedValue({
      SchemaName: "public",
      TableName: "orders",
      Operation: "ADD_COLUMN",
      Strategy: "DIRECT_DDL",
      EstimatedRows: 10,
      Statements: [],
      Warnings: [],
      Notes: [],
    });
  });

  it("populates the Schema dropdown from api.listSchemas on mount", async () => {
    vi.mocked(api.listSchemas).mockResolvedValue(["public", "billing"]);
    vi.mocked(api.listTables).mockResolvedValue([]);
    renderScreen();

    const schemaSelect = await screen.findByRole("combobox", { name: /schema/i });
    await waitFor(() => {
      expect(within(schemaSelect).getByRole("option", { name: "billing" })).toBeInTheDocument();
    });
    expect(within(schemaSelect).getByRole("option", { name: "public" })).toBeInTheDocument();
  });

  it("fetches tables for the default schema (public) on mount", async () => {
    vi.mocked(api.listSchemas).mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockResolvedValue(["orders", "users"]);
    renderScreen();

    await waitFor(() => expect(api.listTables).toHaveBeenCalledWith("public"));
    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => {
      expect(within(tableSelect).getByRole("option", { name: "orders" })).toBeInTheDocument();
    });
  });

  it("re-fetches tables and clears the table field when the schema changes", async () => {
    vi.mocked(api.listSchemas).mockResolvedValue(["public", "billing"]);
    vi.mocked(api.listTables).mockImplementation(async (schema: string) =>
      schema === "public" ? ["orders"] : ["invoices"],
    );
    const user = userEvent.setup();
    renderScreen();

    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");

    const schemaSelect = await screen.findByRole("combobox", { name: /schema/i });
    await user.selectOptions(schemaSelect, "billing");

    await waitFor(() => expect(api.listTables).toHaveBeenCalledWith("billing"));
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "invoices" })).not.toBeNull());
    // The previously-selected table must be cleared, not left pointing at
    // a table from the schema that's no longer selected.
    expect((tableSelect as HTMLSelectElement).value).toBe("");
  });

  it("shows a Column dropdown (not free text) for an operation needing an existing column", async () => {
    vi.mocked(api.listSchemas).mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockResolvedValue(["orders"]);
    vi.mocked(api.listColumns).mockResolvedValue([
      { Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" },
      { Name: "status", Type: "text", Nullable: true, IsPrimaryKey: false, Default: "" },
    ]);
    const user = userEvent.setup();
    renderScreen();

    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, "DROP_COLUMN");

    await waitFor(() => expect(api.listColumns).toHaveBeenCalledWith("public", "orders"));
    const columnSelect = await screen.findByRole("combobox", { name: /^column$/i });
    await waitFor(() => {
      expect(within(columnSelect).getByRole("option", { name: /status — text/ })).toBeInTheDocument();
    });
  });

  it("keeps the Column field as free text for ADD_COLUMN (a new column can't be picked from existing ones)", async () => {
    vi.mocked(api.listSchemas).mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockResolvedValue(["orders"]);
    vi.mocked(api.listColumns).mockResolvedValue([{ Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" }]);
    const user = userEvent.setup();
    renderScreen();

    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");

    // ADD_COLUMN is the default operation — the Column field should be a
    // plain textbox (checking the resolved DOM property, not the
    // attribute: a plain <input> with no explicit type="..." has no
    // "type" ATTRIBUTE at all, but its .type PROPERTY still correctly
    // resolves to "text"). getByRole("textbox", ...) is used rather than
    // getByLabelText here specifically because the latter also matches
    // the Table Overview panel's <table aria-label="Columns">, which is
    // an unrelated ambiguous match, not a real bug.
    await waitFor(() => expect((screen.getByRole("textbox", { name: /column/i }) as HTMLInputElement).type).toBe("text"));
    // listColumns IS called once a table is selected — the table-overview
    // panel needs the full column list regardless of operation — but that
    // must not turn the Column FIELD itself into a dropdown for
    // ADD_COLUMN specifically. Only one "Column" labeled control should
    // exist (the textbox), not two.
    expect(api.listColumns).toHaveBeenCalledWith("public", "orders");
    expect(screen.queryByRole("combobox", { name: /^column$/i })).not.toBeInTheDocument();
  });

  it("shows a retry link and re-fetches when listSchemas fails", async () => {
    vi.mocked(api.listSchemas).mockRejectedValueOnce(new ApiError(500, "database unavailable"));
    vi.mocked(api.listTables).mockResolvedValue([]);
    const user = userEvent.setup();
    renderScreen();

    expect(await screen.findByText("database unavailable")).toBeInTheDocument();

    vi.mocked(api.listSchemas).mockResolvedValueOnce(["public"]);
    await user.click(screen.getByRole("button", { name: /retry/i }));

    await waitFor(() => expect(api.listSchemas).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText("database unavailable")).not.toBeInTheDocument());
  });

  // Schema/table/column errors could all be visible at once (e.g. a
  // database blip right as the operator is filling the form out) — each
  // Retry button needs a distinct accessible name, or a screen reader
  // user tabbing through them would hear "Retry, Retry, Retry" with no
  // way to tell which failed list each one reloads.
  it("gives each Retry button a distinct accessible name naming what it reloads", async () => {
    vi.mocked(api.listSchemas).mockRejectedValue(new ApiError(500, "schemas failed"));
    vi.mocked(api.listTables).mockResolvedValue([]);
    renderScreen();

    const retrySchemas = await screen.findByRole("button", { name: /retry loading schemas/i });
    expect(retrySchemas).toBeInTheDocument();
  });
});

describe("NewMigration — connection info banner", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset().mockResolvedValue([]);
    vi.mocked(api.listTables).mockReset().mockResolvedValue([]);
    vi.mocked(api.listColumns).mockReset().mockResolvedValue([]);
    vi.mocked(api.sampleRows).mockReset().mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    vi.mocked(api.getStrategyMatrix).mockReset().mockResolvedValue({});
  });

  it("shows the read-only host/port/database/username the server is connected to", async () => {
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    renderScreen();

    expect(await screen.findByText("Hostname/IP")).toBeInTheDocument();
    expect(screen.getByText("db.internal")).toBeInTheDocument();
    expect(screen.getByText("5432")).toBeInTheDocument();
    expect(screen.getByText("orders_prod")).toBeInTheDocument();
    expect(screen.getByText("pgarchimigrator")).toBeInTheDocument();
  });

  it("has no editable inputs for the connection info — display only", async () => {
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    renderScreen();

    await screen.findByText("Hostname/IP");
    // None of Host/Port/Username should ever appear as a form control's
    // accessible name — this is a display banner, not a form.
    expect(screen.queryByLabelText(/host/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^port$/i)).not.toBeInTheDocument();
  });

  it("shows the PostgreSQL major version with no badge when it's within the supported range", async () => {
    vi.mocked(api.getConnectionInfo).mockReset().mockResolvedValue({
      Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod",
      PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4 on x86_64-pc-linux-gnu", VersionSupportStatus: "supported",
    });
    renderScreen();

    expect(await screen.findByText("PostgreSQL 16")).toBeInTheDocument();
    expect(screen.queryByText("unsupported version")).not.toBeInTheDocument();
    expect(screen.queryByText("newer than tested")).not.toBeInTheDocument();
  });

  it("shows a warning badge when the version is newer than this project has tested against", async () => {
    vi.mocked(api.getConnectionInfo).mockReset().mockResolvedValue({
      Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod",
      PostgresVersion: 20, PostgresVersionString: "PostgreSQL 20.0", VersionSupportStatus: "newer_than_tested",
    });
    renderScreen();

    expect(await screen.findByText("PostgreSQL 20")).toBeInTheDocument();
    expect(screen.getByText("newer than tested")).toBeInTheDocument();
  });

  it("shows an unsupported-version badge when the version is below the minimum", async () => {
    vi.mocked(api.getConnectionInfo).mockReset().mockResolvedValue({
      Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod",
      PostgresVersion: 11, PostgresVersionString: "PostgreSQL 11.2", VersionSupportStatus: "below_minimum",
    });
    renderScreen();

    expect(await screen.findByText("PostgreSQL 11")).toBeInTheDocument();
    expect(screen.getByText("unsupported version")).toBeInTheDocument();
  });

  it("shows no version badge at all while the version hasn't been determined yet (PostgresVersion=0)", async () => {
    vi.mocked(api.getConnectionInfo).mockReset().mockResolvedValue({
      Host: "db.internal", Port: 5432, Username: "pgarchimigrator", Database: "orders_prod",
      PostgresVersion: 0, PostgresVersionString: "", VersionSupportStatus: "",
    });
    renderScreen();

    await screen.findByText("Hostname/IP");
    expect(screen.queryByText(/PostgreSQL \d/)).not.toBeInTheDocument();
  });
});

describe("NewMigration — table overview panel", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset().mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockReset().mockResolvedValue(["orders"]);
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "localhost", Port: 5432, Username: "pgarchimigrator", Database: "pgarchimigrator_test", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    vi.mocked(api.getStrategyMatrix).mockReset().mockResolvedValue({});
  });

  async function selectOrdersTable(user: ReturnType<typeof userEvent.setup>) {
    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");
  }

  it("does not show the table overview panel before a table is selected", async () => {
    vi.mocked(api.listColumns).mockResolvedValue([]);
    vi.mocked(api.sampleRows).mockResolvedValue({ Columns: [], Rows: [] });
    renderScreen();

    await screen.findByRole("combobox", { name: /^table$/i });
    expect(screen.queryByRole("table", { name: "Columns" })).not.toBeInTheDocument();
  });

  it("shows the column count, names, types, PK badge, nullability, and defaults once a table is selected", async () => {
    vi.mocked(api.listColumns).mockResolvedValue([
      { Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" },
      { Name: "status", Type: "text", Nullable: false, IsPrimaryKey: false, Default: "'active'::text" },
    ]);
    vi.mocked(api.sampleRows).mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.getTableStats).mockResolvedValue({
      SchemaName: "public", TableName: "orders", EstimatedRowCount: 42_000, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT",
    });
    const user = userEvent.setup();
    renderScreen();

    await selectOrdersTable(user);

    // "2 columns" and the row count now render inside the same element
    // (see NewMigration.tsx's table overview header) — a substring
    // match, not the old exact-text match, since the row count text
    // ("· 42,000 rows (est.)") is part of the same text node.
    expect(await screen.findByText(/2 columns/)).toBeInTheDocument();
    expect(screen.getByText(/42,000 rows \(est\.\)/)).toBeInTheDocument();
    expect(screen.getByText("id")).toBeInTheDocument();
    expect(screen.getByText("PK")).toBeInTheDocument();
    expect(screen.getByText("bigint")).toBeInTheDocument();
    expect(screen.getByText("'active'::text")).toBeInTheDocument();
    // "id" is non-nullable and has no PK badge conflict with "status" —
    // both NOT NULL cells should render, distinguishing them from a
    // nullable column would need a real nullable example, covered next.
    expect(screen.getAllByText("NOT NULL").length).toBe(2);
  });

  it("shows up to 5 sample rows with real cell values", async () => {
    vi.mocked(api.listColumns).mockResolvedValue([{ Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" }]);
    vi.mocked(api.sampleRows).mockResolvedValue({
      Columns: ["id", "label"],
      Rows: [
        ["1", "first"],
        ["2", "second"],
      ],
    });
    const user = userEvent.setup();
    renderScreen();

    await selectOrdersTable(user);

    expect(await screen.findByText("first")).toBeInTheDocument();
    expect(screen.getByText("second")).toBeInTheDocument();
  });

  it("shows an empty-table message when the table has no rows", async () => {
    vi.mocked(api.listColumns).mockResolvedValue([{ Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" }]);
    vi.mocked(api.sampleRows).mockResolvedValue({ Columns: ["id"], Rows: [] });
    const user = userEvent.setup();
    renderScreen();

    await selectOrdersTable(user);

    expect(await screen.findByText("This table is empty.")).toBeInTheDocument();
  });

  it("does not show a row count before getTableStats resolves, but shows it once it does", async () => {
    vi.mocked(api.listColumns).mockResolvedValue([{ Name: "id", Type: "bigint", Nullable: false, IsPrimaryKey: true, Default: "" }]);
    vi.mocked(api.getTableStats).mockResolvedValue({
      SchemaName: "public", TableName: "orders", EstimatedRowCount: 10_000_000, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT",
    });
    const user = userEvent.setup();
    renderScreen();

    await selectOrdersTable(user);

    expect(await screen.findByText(/10,000,000 rows \(est\.\)/)).toBeInTheDocument();
  });
});

describe("NewMigration — strategy override restricted by operation", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset().mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockReset().mockResolvedValue(["orders"]);
    vi.mocked(api.listColumns).mockReset().mockResolvedValue([]);
    vi.mocked(api.sampleRows).mockReset().mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "localhost", Port: 5432, Username: "pgarchimigrator", Database: "pgarchimigrator_test", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    // A real (non-empty) matrix — the whole point of this describe
    // block is proving the dropdown actually filters against it, unlike
    // the outer default (an empty object, harmless everywhere else that
    // doesn't care about this specific restriction).
    vi.mocked(api.getStrategyMatrix)
      .mockReset()
      .mockResolvedValue({
        ADD_COLUMN: ["DIRECT_DDL", "EXPAND_BACKFILL"],
        ADD_INDEX: ["DIRECT_DDL"],
        ALTER_COLUMN_TYPE: ["DIRECT_DDL", "SHADOW_TABLE"],
      });
  });

  // Direct regression test for the real incident that prompted this
  // whole feature: ADD_INDEX forced through SHADOW_TABLE used to be
  // silently accepted by the UI (every strategy was always shown,
  // regardless of operation), then silently did nothing useful server-side
  // (see internal/strategy's validStrategiesByOperation doc comment for
  // the full incident — the requested index was never created, but the
  // migration reported COMPLETED anyway).
  it("shows only the single valid strategy for ADD_INDEX — no separate (automatic) option, since it would mean exactly the same thing", async () => {
    const user = userEvent.setup();
    renderScreen();

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, "ADD_INDEX");

    await waitFor(() => {
      const strategySelect = screen.getByRole("combobox", { name: /strategy override/i });
      const optionValues = within(strategySelect)
        .getAllByRole("option")
        .map((o) => (o as HTMLOptionElement).value);
      // Just DIRECT_DDL — no "" (automatic) alongside it, and
      // definitely not SHADOW_TABLE (the real incident this whole
      // feature exists to prevent).
      expect(optionValues).toEqual(["DIRECT_DDL"]);
      expect(strategySelect).toHaveValue("DIRECT_DDL");
    });
  });

  it("shows SHADOW_TABLE as a valid choice for ALTER_COLUMN_TYPE, alongside (automatic) since two strategies are genuinely possible", async () => {
    const user = userEvent.setup();
    renderScreen();

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, "ALTER_COLUMN_TYPE");

    await waitFor(() => {
      const strategySelect = screen.getByRole("combobox", { name: /strategy override/i });
      const optionValues = within(strategySelect)
        .getAllByRole("option")
        .map((o) => (o as HTMLOptionElement).value);
      expect(optionValues).toEqual(["", "DIRECT_DDL", "SHADOW_TABLE"]);
    });
  });

  it("resets a now-invalid strategy override when switching to an operation with a different single valid strategy", async () => {
    const user = userEvent.setup();
    renderScreen();

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, "ALTER_COLUMN_TYPE");

    const strategySelect = await screen.findByRole("combobox", { name: /strategy override/i });
    await user.selectOptions(strategySelect, "SHADOW_TABLE");
    expect(strategySelect).toHaveValue("SHADOW_TABLE");

    // ADD_INDEX's whitelist is just [DIRECT_DDL] — switching to it must
    // reset the now-invalid SHADOW_TABLE selection, landing on
    // DIRECT_DDL explicitly (the single valid choice, auto-selected —
    // see the previous test) rather than silently carrying the stale,
    // invalid value forward.
    await user.selectOptions(operationSelect, "ADD_INDEX");

    await waitFor(() => expect(strategySelect).toHaveValue("DIRECT_DDL"));
  });

  // Direct regression test for a real inconsistency a user caught: an
  // EXPLICIT strategy chosen for one operation used to silently carry
  // over to a completely different operation whenever that strategy
  // happened to ALSO be valid there (e.g. DIRECT_DDL is valid for almost
  // everything) — landing on a specific forced strategy the user never
  // actually chose FOR this new operation, instead of a fresh
  // "(automatic)" default. An operation change should always reset to
  // "(automatic)" unless only one strategy is possible at all (that
  // case is covered by the single-option tests above).
  it("resets to (automatic) on an operation change even when the previous explicit choice would still be valid", async () => {
    const user = userEvent.setup();
    renderScreen();

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    // ADD_COLUMN's whitelist includes DIRECT_DDL (see this file's
    // default getStrategyMatrix mock) — pick it explicitly.
    const strategySelect = await screen.findByRole("combobox", { name: /strategy override/i });
    await user.selectOptions(strategySelect, "DIRECT_DDL");
    expect(strategySelect).toHaveValue("DIRECT_DDL");

    // ALTER_COLUMN_TYPE's whitelist ALSO includes DIRECT_DDL — the
    // explicit choice above would technically still be "valid", but it
    // was never actually chosen for THIS operation.
    await user.selectOptions(operationSelect, "ALTER_COLUMN_TYPE");

    await waitFor(() => expect(strategySelect).toHaveValue(""));
  });
});

describe("NewMigration — write load check (SHADOW_TABLE only)", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset().mockResolvedValue(["public"]);
    vi.mocked(api.listTables).mockReset().mockResolvedValue(["orders"]);
    vi.mocked(api.listColumns)
      .mockReset()
      .mockResolvedValue([{ Name: "customer_id", Type: "integer", Nullable: false, IsPrimaryKey: false, Default: "" }]);
    vi.mocked(api.sampleRows).mockReset().mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "localhost", Port: 5432, Username: "pgarchimigrator", Database: "pgarchimigrator_test", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    vi.mocked(api.getStrategyMatrix)
      .mockReset()
      .mockResolvedValue({ ALTER_COLUMN_TYPE: ["DIRECT_DDL", "SHADOW_TABLE"] });
    vi.mocked(api.estimateWriteLoad).mockReset();
  });

  async function fillFormToShadowTablePreview(user: ReturnType<typeof userEvent.setup>, strategy: string) {
    vi.mocked(api.previewMigration).mockReset().mockResolvedValue({
      SchemaName: "public", TableName: "orders", Operation: "ALTER_COLUMN_TYPE",
      Strategy: strategy, EstimatedRows: 10, Statements: [], Warnings: [], Notes: [],
    });
    renderScreen();

    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, "ALTER_COLUMN_TYPE");

    const columnSelect = await screen.findByRole("combobox", { name: /column/i });
    await waitFor(() => expect(within(columnSelect).queryByRole("option", { name: /customer_id/i })).not.toBeNull());
    await user.selectOptions(columnSelect, "customer_id");

    const typeField = screen.getByRole("textbox", { name: /^type/i });
    await user.type(typeField, "text");

    await waitFor(() => expect(screen.queryByText("Generating preview…")).not.toBeInTheDocument());
  }

  it("does not show the write-load check for a DIRECT_DDL preview", async () => {
    const user = userEvent.setup();
    await fillFormToShadowTablePreview(user, "DIRECT_DDL");

    expect(screen.queryByRole("button", { name: /check current write load/i })).not.toBeInTheDocument();
  });

  it("shows the write-load check button for a SHADOW_TABLE preview, with no result until clicked", async () => {
    const user = userEvent.setup();
    await fillFormToShadowTablePreview(user, "SHADOW_TABLE");

    expect(screen.getByRole("button", { name: /check current write load/i })).toBeInTheDocument();
    expect(api.estimateWriteLoad).not.toHaveBeenCalled();
  });

  it("shows a loading state, then the rate, after clicking the check button", async () => {
    const user = userEvent.setup();
    await fillFormToShadowTablePreview(user, "SHADOW_TABLE");

    let resolveEstimate: (v: { bytesPerSecond: number; sampleSeconds: number; caution: boolean }) => void;
    vi.mocked(api.estimateWriteLoad).mockReturnValue(
      new Promise((resolve) => {
        resolveEstimate = resolve;
      }),
    );

    await user.click(screen.getByRole("button", { name: /check current write load/i }));
    expect(await screen.findByText(/measuring for 10 seconds/i)).toBeInTheDocument();

    resolveEstimate!({ bytesPerSecond: 1_048_576, sampleSeconds: 10, caution: false });

    expect(await screen.findByText(/1\.0 MB\/s/)).toBeInTheDocument();
    expect(screen.queryByText(/genuinely busy database/i)).not.toBeInTheDocument();
  });

  // Direct regression test for the advisory-only framing: the caution
  // message must appear when caution=true, and must never claim the
  // migration will be blocked or stopped automatically.
  it("shows a caution note (not a blocking error) when the sampled rate crosses the threshold", async () => {
    const user = userEvent.setup();
    await fillFormToShadowTablePreview(user, "SHADOW_TABLE");
    vi.mocked(api.estimateWriteLoad).mockResolvedValue({ bytesPerSecond: 8_388_608, sampleSeconds: 10, caution: true });

    await user.click(screen.getByRole("button", { name: /check current write load/i }));

    expect(await screen.findByText(/genuinely busy database/i)).toBeInTheDocument();
    // The Start button (however it's labeled) must remain present and
    // enabled — this is advisory, never a hard block.
    expect(screen.getByRole("button", { name: /start migration/i })).toBeEnabled();
  });

  it("shows an error message if the write-load check itself fails", async () => {
    const user = userEvent.setup();
    await fillFormToShadowTablePreview(user, "SHADOW_TABLE");
    vi.mocked(api.estimateWriteLoad).mockRejectedValue(new ApiError(500, "could not sample WAL position"));

    await user.click(screen.getByRole("button", { name: /check current write load/i }));

    expect(await screen.findByText("could not sample WAL position")).toBeInTheDocument();
  });
});

describe("NewMigration — new operations (RENAME_TABLE, ADD_FOREIGN_KEY, ADD_GENERATED_COLUMN, PARTITION_TABLE)", () => {
  beforeEach(() => {
    vi.mocked(api.me).mockReset().mockResolvedValue({ id: "u1", email: "op@b.com", role: "operator" });
    vi.mocked(api.listSchemas).mockReset().mockResolvedValue(["public"]);
    // Two tables — ADD_FOREIGN_KEY's own "Referenced table" dropdown
    // filters out whichever table is currently the SOURCE table (see
    // NewMigration.tsx's own comment on that dropdown), so a single
    // "orders" table alone would leave nothing selectable there.
    vi.mocked(api.listTables).mockReset().mockResolvedValue(["orders", "customers", "products"]);
    vi.mocked(api.listColumns)
      .mockReset()
      .mockImplementation(async (_schema: string, table: string) => {
        if (table === "customers") {
          return [
            { Name: "id", Type: "integer", Nullable: false, IsPrimaryKey: true, Default: "" },
            { Name: "name", Type: "text", Nullable: false, IsPrimaryKey: false, Default: "" },
          ];
        }
        if (table === "products") {
          return [{ Name: "sku", Type: "text", Nullable: false, IsPrimaryKey: true, Default: "" }];
        }
        return [{ Name: "customer_id", Type: "integer", Nullable: false, IsPrimaryKey: false, Default: "" }];
      });
    vi.mocked(api.sampleRows).mockReset().mockResolvedValue({ Columns: [], Rows: [] });
    vi.mocked(api.getConnectionInfo)
      .mockReset()
      .mockResolvedValue({ Host: "localhost", Port: 5432, Username: "pgarchimigrator", Database: "pgarchimigrator_test", PostgresVersion: 16, PostgresVersionString: "PostgreSQL 16.4", VersionSupportStatus: "supported" });
    vi.mocked(api.getTableStats)
      .mockReset()
      .mockResolvedValue({ SchemaName: "public", TableName: "orders", EstimatedRowCount: 0, IsPartitioned: false, HasPrimaryKey: true, ReplicaIdentity: "DEFAULT" });
    vi.mocked(api.getStrategyMatrix)
      .mockReset()
      .mockResolvedValue({
        RENAME_TABLE: ["DIRECT_DDL"],
        ADD_FOREIGN_KEY: ["DIRECT_DDL"],
        ADD_GENERATED_COLUMN: ["DIRECT_DDL", "EXPAND_BACKFILL"],
        PARTITION_TABLE: ["SHADOW_TABLE"],
      });
    vi.mocked(api.previewMigration).mockReset();
  });

  async function selectTableAndOperation(user: ReturnType<typeof userEvent.setup>, operation: string) {
    renderScreen();
    const tableSelect = await screen.findByRole("combobox", { name: /^table$/i });
    await waitFor(() => expect(within(tableSelect).queryByRole("option", { name: "orders" })).not.toBeNull());
    await user.selectOptions(tableSelect, "orders");

    const operationSelect = await screen.findByRole("combobox", { name: /operation/i });
    await user.selectOptions(operationSelect, operation);
  }

  it("shows the New table name field for RENAME_TABLE, and no Column field at all", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "RENAME_TABLE");

    expect(await screen.findByRole("textbox", { name: /new table name/i })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /^column$/i })).not.toBeInTheDocument();
  });

  // Direct regression test for the full RENAME_TABLE flow actually
  // reaching a preview — filling in just the one required field
  // (new_table_name, no column at all) must be enough to trigger
  // previewMigration.
  it("triggers a preview for RENAME_TABLE with only the new table name filled in", async () => {
    vi.mocked(api.previewMigration).mockResolvedValue({
      SchemaName: "public", TableName: "orders", Operation: "RENAME_TABLE",
      Strategy: "DIRECT_DDL", EstimatedRows: 10, Statements: [], Warnings: [], Notes: [],
    });
    const user = userEvent.setup();
    await selectTableAndOperation(user, "RENAME_TABLE");

    const newTableNameField = await screen.findByRole("textbox", { name: /new table name/i });
    await user.type(newTableNameField, "orders_v2");

    await waitFor(() => expect(api.previewMigration).toHaveBeenCalled());
  });

  it("shows constraint name, referenced table/column dropdowns (excluding the source table), and an on-delete dropdown for ADD_FOREIGN_KEY", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "ADD_FOREIGN_KEY");

    expect(await screen.findByRole("textbox", { name: /constraint name/i })).toBeInTheDocument();

    // Direct regression test for the actual ask: Referenced table is a
    // dropdown of REAL existing tables, excluding "orders" (the
    // already-selected SOURCE table) — not a free-text field.
    const referencedTableSelect = screen.getByRole("combobox", { name: /referenced table/i });
    expect(within(referencedTableSelect).getByRole("option", { name: "customers" })).toBeInTheDocument();
    expect(within(referencedTableSelect).queryByRole("option", { name: "orders" })).not.toBeInTheDocument();

    await user.selectOptions(referencedTableSelect, "customers");

    // Direct regression test for Referenced column being populated from
    // the CHOSEN referenced table's own real columns, with the primary
    // key ("id") clearly marked.
    const referencedColumnSelect = await screen.findByRole("combobox", { name: /referenced column/i });
    expect(await within(referencedColumnSelect).findByRole("option", { name: /id — primary key/i })).toBeInTheDocument();
    expect(within(referencedColumnSelect).getByRole("option", { name: "name" })).toBeInTheDocument();

    const onDeleteSelect = screen.getByRole("combobox", { name: /on delete/i });
    expect(within(onDeleteSelect).getByRole("option", { name: "CASCADE" })).toBeInTheDocument();
  });

  // Direct regression test for changing the referenced table resetting
  // any previously chosen referenced column — an already-selected
  // column from the OLD referenced table would otherwise silently
  // remain selected even though it may not even exist on the new one.
  it("resets the referenced column when the referenced table is changed", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "ADD_FOREIGN_KEY");

    const referencedTableSelect = screen.getByRole("combobox", { name: /referenced table/i });
    await user.selectOptions(referencedTableSelect, "customers");

    const referencedColumnSelect = await screen.findByRole("combobox", { name: /referenced column/i });
    await user.selectOptions(referencedColumnSelect, "id");
    expect(referencedColumnSelect).toHaveValue("id");

    // Switch to a genuinely DIFFERENT referenced table ("products", not
    // "customers" again) — re-selecting the SAME value wouldn't
    // reliably fire onChange at all, so this needs a real change to
    // actually exercise the reset logic.
    await user.selectOptions(referencedTableSelect, "products");
    expect(referencedColumnSelect).toHaveValue("");
  });

  it("shows the Generated expression field for ADD_GENERATED_COLUMN, and a free-text Column field (not a dropdown)", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "ADD_GENERATED_COLUMN");

    expect(await screen.findByRole("textbox", { name: /generated expression/i })).toBeInTheDocument();
    // The column here is being CREATED (like ADD_COLUMN's own column
    // field), so it must be free text, not a dropdown fed by existing
    // columns — see needsExistingColumn's own doc comment.
    expect(screen.getByRole("textbox", { name: /^column/i })).toBeInTheDocument();
  });

  it("shows partition column, strategy, bounds, and the DEFAULT-partition checkbox for PARTITION_TABLE — and no Column field", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "PARTITION_TABLE");

    expect(await screen.findByRole("textbox", { name: /partition column/i })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: /partition strategy/i })).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: /DEFAULT partition/i })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: /^column$/i })).not.toBeInTheDocument();
  });

  // Direct regression test for the rule-based shortcut's own visibility
  // rule: it must only appear for RANGE (LIST has no equivalent — see
  // strategy.ExpandPartitionRule's own doc comment), not unconditionally
  // once PARTITION_TABLE is selected.
  it("only shows the interval rule shortcut once RANGE is selected as the partition strategy, not for LIST", async () => {
    const user = userEvent.setup();
    await selectTableAndOperation(user, "PARTITION_TABLE");
    await screen.findByRole("textbox", { name: /partition column/i });

    expect(screen.queryByRole("combobox", { name: /^interval$/i })).not.toBeInTheDocument();

    const strategySelect = screen.getByRole("combobox", { name: /partition strategy/i });
    await user.selectOptions(strategySelect, "LIST");
    expect(screen.queryByRole("combobox", { name: /^interval$/i })).not.toBeInTheDocument();

    await user.selectOptions(strategySelect, "RANGE");
    expect(screen.getByRole("combobox", { name: /^interval$/i })).toBeInTheDocument();
  });

  // Direct regression test for the full PARTITION_TABLE flow reaching a
  // preview via the rule-based shortcut specifically (not explicit JSON
  // bounds) — proving isReadyForPreview's rule-completeness check and
  // the actual form wiring agree with each other end to end.
  it("triggers a preview for PARTITION_TABLE using the rule-based shortcut, with no explicit JSON bounds typed in", async () => {
    vi.mocked(api.previewMigration).mockResolvedValue({
      SchemaName: "public", TableName: "orders", Operation: "PARTITION_TABLE",
      Strategy: "SHADOW_TABLE", EstimatedRows: 10, Statements: [], Warnings: [], Notes: [],
    });
    const user = userEvent.setup();
    await selectTableAndOperation(user, "PARTITION_TABLE");

    await user.type(await screen.findByRole("textbox", { name: /partition column/i }), "created_at");
    await user.selectOptions(screen.getByRole("combobox", { name: /partition strategy/i }), "RANGE");
    await user.selectOptions(screen.getByRole("combobox", { name: /^interval$/i }), "monthly");
    await user.type(screen.getByRole("textbox", { name: /^from$/i }), "2024-01-01");
    await user.type(screen.getByRole("textbox", { name: /^to$/i }), "2024-04-01");

    await waitFor(() => expect(api.previewMigration).toHaveBeenCalled());
    const calls = vi.mocked(api.previewMigration).mock.calls;
    const call = calls[calls.length - 1]?.[0];
    expect(call?.partition_bounds).toBeUndefined();
    expect(call?.partition_interval).toBe("monthly");
  });
});
