import { describe, expect, it } from "vitest";
import { initialForm, isReadyForPreview, needsExistingColumn, type FormState } from "./NewMigration";
import type { Operation } from "../lib/types";

// build() starts from a fully valid ADD_COLUMN form and applies overrides —
// this way each test only states what's actually being varied, matching
// the intent of the field it's testing rather than restating every field.
function build(overrides: Partial<FormState>): FormState {
  return { ...initialForm, table: "orders", column: "status", ...overrides };
}

// This test suite is a direct regression guard for the comment on
// isReadyForPreview itself: it must stay in sync with
// internal/api/server.go's buildMigrationRequest validation (see
// cmd/pgarchimigrator/main.go for the CLI's identical rules). Each case here
// mirrors one branch of that Go switch statement.
describe("isReadyForPreview", () => {
  it("is never ready without a table name, regardless of operation", () => {
    expect(isReadyForPreview(build({ table: "", operation: "ADD_COLUMN" }))).toBe(false);
    expect(isReadyForPreview(build({ table: "  ", operation: "DROP_INDEX", index_name: "idx1" }))).toBe(false);
  });

  describe("ADD_COLUMN / DROP_COLUMN / ADD_INDEX / SET_NOT_NULL (default: column required)", () => {
    for (const op of ["ADD_COLUMN", "DROP_COLUMN", "ADD_INDEX", "SET_NOT_NULL"] as const) {
      it(`${op}: ready once column is filled`, () => {
        expect(isReadyForPreview(build({ operation: op, column: "status" }))).toBe(true);
      });
      it(`${op}: not ready with an empty column`, () => {
        expect(isReadyForPreview(build({ operation: op, column: "" }))).toBe(false);
      });
      it(`${op}: not ready with a whitespace-only column`, () => {
        expect(isReadyForPreview(build({ operation: op, column: "   " }))).toBe(false);
      });
    }
  });

  describe("DROP_INDEX (index_name required, column NOT required)", () => {
    it("is ready with only an index name, no column", () => {
      expect(isReadyForPreview(build({ operation: "DROP_INDEX", column: "", index_name: "idx_orders_status" }))).toBe(
        true,
      );
    });
    it("is not ready without an index name", () => {
      expect(isReadyForPreview(build({ operation: "DROP_INDEX", index_name: "" }))).toBe(false);
    });
  });

  describe("ADD_CONSTRAINT (constraint_name AND check_expression required, column NOT required)", () => {
    it("is ready with both fields filled, no column", () => {
      expect(
        isReadyForPreview(
          build({ operation: "ADD_CONSTRAINT", column: "", constraint_name: "price_check", check_expression: "price > 0" }),
        ),
      ).toBe(true);
    });
    it("is not ready with only constraint_name", () => {
      expect(
        isReadyForPreview(build({ operation: "ADD_CONSTRAINT", constraint_name: "price_check", check_expression: "" })),
      ).toBe(false);
    });
    it("is not ready with only check_expression", () => {
      expect(
        isReadyForPreview(build({ operation: "ADD_CONSTRAINT", constraint_name: "", check_expression: "price > 0" })),
      ).toBe(false);
    });
  });

  describe("RENAME_COLUMN (column AND new_column_name required)", () => {
    it("is ready with both the old and new names filled", () => {
      expect(
        isReadyForPreview(build({ operation: "RENAME_COLUMN", column: "old_name", new_column_name: "new_name" })),
      ).toBe(true);
    });
    it("is not ready with only the old name", () => {
      expect(isReadyForPreview(build({ operation: "RENAME_COLUMN", column: "old_name", new_column_name: "" }))).toBe(
        false,
      );
    });
    it("is not ready with only the new name", () => {
      expect(isReadyForPreview(build({ operation: "RENAME_COLUMN", column: "", new_column_name: "new_name" }))).toBe(
        false,
      );
    });
  });

  describe("ALTER_COLUMN_TYPE (column AND type required)", () => {
    it("is ready with both column and type filled", () => {
      expect(isReadyForPreview(build({ operation: "ALTER_COLUMN_TYPE", column: "amount", type: "numeric(12,2)" }))).toBe(
        true,
      );
    });
    it("is not ready with only the column", () => {
      expect(isReadyForPreview(build({ operation: "ALTER_COLUMN_TYPE", column: "amount", type: "" }))).toBe(false);
    });
    it("is not ready with only the type", () => {
      expect(isReadyForPreview(build({ operation: "ALTER_COLUMN_TYPE", column: "", type: "numeric(12,2)" }))).toBe(
        false,
      );
    });
  });

  describe("RENAME_TABLE (new_table_name required, no column needed)", () => {
    it("is ready with a new table name, even with no column at all", () => {
      expect(isReadyForPreview(build({ operation: "RENAME_TABLE", column: "", new_table_name: "orders_v2" }))).toBe(
        true,
      );
    });
    it("is not ready without a new table name", () => {
      expect(isReadyForPreview(build({ operation: "RENAME_TABLE", new_table_name: "" }))).toBe(false);
    });
  });

  describe("ADD_FOREIGN_KEY (column, constraint_name, referenced_table, referenced_column all required)", () => {
    function fkForm(overrides: Partial<FormState> = {}): FormState {
      return build({
        operation: "ADD_FOREIGN_KEY",
        column: "customer_id",
        constraint_name: "fk_orders_customer",
        referenced_table: "customers",
        referenced_column: "id",
        ...overrides,
      });
    }
    it("is ready with all four fields filled", () => {
      expect(isReadyForPreview(fkForm())).toBe(true);
    });
    it("is not ready without a local column", () => {
      expect(isReadyForPreview(fkForm({ column: "" }))).toBe(false);
    });
    it("is not ready without a constraint name", () => {
      expect(isReadyForPreview(fkForm({ constraint_name: "" }))).toBe(false);
    });
    it("is not ready without a referenced table", () => {
      expect(isReadyForPreview(fkForm({ referenced_table: "" }))).toBe(false);
    });
    it("is not ready without a referenced column", () => {
      expect(isReadyForPreview(fkForm({ referenced_column: "" }))).toBe(false);
    });
    // on_delete is deliberately NOT required — see the form's own
    // "(default — NO ACTION)" option, matching internal/api's optional
    // treatment of this field.
    it("is ready with on_delete left empty", () => {
      expect(isReadyForPreview(fkForm({ on_delete: "" }))).toBe(true);
    });
  });

  describe("ADD_GENERATED_COLUMN (column, type, generated_expression all required)", () => {
    function generatedForm(overrides: Partial<FormState> = {}): FormState {
      return build({
        operation: "ADD_GENERATED_COLUMN",
        column: "total",
        type: "numeric",
        generated_expression: "price * quantity",
        ...overrides,
      });
    }
    it("is ready with all three fields filled", () => {
      expect(isReadyForPreview(generatedForm())).toBe(true);
    });
    it("is not ready without a column", () => {
      expect(isReadyForPreview(generatedForm({ column: "" }))).toBe(false);
    });
    it("is not ready without a type", () => {
      expect(isReadyForPreview(generatedForm({ type: "" }))).toBe(false);
    });
    it("is not ready without a generated expression", () => {
      expect(isReadyForPreview(generatedForm({ generated_expression: "" }))).toBe(false);
    });
  });

  describe("PARTITION_TABLE (partition_column + partition_strategy + EITHER explicit bounds OR a complete rule)", () => {
    it("is not ready without a partition column, even with everything else filled", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "",
            partition_strategy: "RANGE",
            partition_bounds_raw: '[{"name":"p1","from":"a","to":"b"}]',
          }),
        ),
      ).toBe(false);
    });
    it("is not ready without a partition strategy", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "created_at",
            partition_strategy: "",
            partition_bounds_raw: '[{"name":"p1","from":"a","to":"b"}]',
          }),
        ),
      ).toBe(false);
    });
    it("is ready with explicit JSON bounds and no rule fields at all", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "region",
            partition_strategy: "LIST",
            partition_bounds_raw: '[{"name":"orders_eu","values":["DE","FR"]}]',
          }),
        ),
      ).toBe(true);
    });
    // Direct regression test for the rule-based shortcut actually being
    // a genuine alternative to typing out JSON bounds by hand — see
    // strategy.ExpandPartitionRule's own doc comment for why this only
    // applies to RANGE.
    it("is ready with a complete rule and no explicit bounds at all", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "created_at",
            partition_strategy: "RANGE",
            partition_bounds_raw: "",
            partition_interval: "monthly",
            partition_rule_from: "2024-01-01",
            partition_rule_to: "2024-04-01",
          }),
        ),
      ).toBe(true);
    });
    it("is not ready with an incomplete rule (missing partition_rule_to) and no explicit bounds", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "created_at",
            partition_strategy: "RANGE",
            partition_bounds_raw: "",
            partition_interval: "monthly",
            partition_rule_from: "2024-01-01",
            partition_rule_to: "",
          }),
        ),
      ).toBe(false);
    });
    it("is not ready with neither explicit bounds nor any rule fields", () => {
      expect(
        isReadyForPreview(
          build({
            operation: "PARTITION_TABLE",
            column: "",
            partition_column: "created_at",
            partition_strategy: "RANGE",
            partition_bounds_raw: "",
          }),
        ),
      ).toBe(false);
    });
  });
});

// This mirrors the same DecisionMatrix-style approach as isReadyForPreview's
// suite above: one row per operation, since needsExistingColumn's whole
// job is a per-operation decision and a table of every case is the most
// direct way to guard it against regressing on any single one.
describe("needsExistingColumn", () => {
  const cases: Array<[Operation, boolean]> = [
    ["ADD_COLUMN", false], // the column is being CREATED — can't pick from existing ones
    ["DROP_INDEX", false], // no column field is shown at all for this operation
    ["ADD_CONSTRAINT", false], // no column field is shown at all for this operation
    ["RENAME_TABLE", false], // acts on the table itself, no column field at all
    ["PARTITION_TABLE", false], // acts on the table itself, no column field at all
    ["ADD_GENERATED_COLUMN", false], // the column is being CREATED — can't pick from existing ones
    ["DROP_COLUMN", true],
    ["ALTER_COLUMN_TYPE", true],
    ["ADD_INDEX", true],
    ["SET_NOT_NULL", true],
    ["RENAME_COLUMN", true], // the CURRENT column name must be an existing one
    ["ADD_FOREIGN_KEY", true], // the LOCAL column must be an existing one
  ];

  for (const [op, want] of cases) {
    it(`${op} -> ${want}`, () => {
      expect(needsExistingColumn(op)).toBe(want);
    });
  }
});
