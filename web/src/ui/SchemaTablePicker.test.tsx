import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SchemaTablePicker, tableKey } from "./SchemaTablePicker";
import type { IntrospectedSchema } from "../lib/types";

const schemas: IntrospectedSchema[] = [
  { name: "public", tables: ["orders", "customers"] },
  { name: "billing", tables: ["invoices"] },
];

describe("tableKey", () => {
  it("joins schema and table with a dot", () => {
    expect(tableKey("public", "orders")).toBe("public.orders");
  });
});

describe("SchemaTablePicker", () => {
  it("renders every schema and table", () => {
    render(<SchemaTablePicker schemas={schemas} selected={new Set()} onChange={vi.fn()} />);

    expect(screen.getByText("public")).toBeInTheDocument();
    expect(screen.getByText("billing")).toBeInTheDocument();
    expect(screen.getByText("orders")).toBeInTheDocument();
    expect(screen.getByText("customers")).toBeInTheDocument();
    expect(screen.getByText("invoices")).toBeInTheDocument();
  });

  it("shows the selected count for each schema", () => {
    render(
      <SchemaTablePicker
        schemas={schemas}
        selected={new Set([tableKey("public", "orders")])}
        onChange={vi.fn()}
      />,
    );

    expect(screen.getByText("(1/2 tables)")).toBeInTheDocument();
    expect(screen.getByText("(0/1 tables)")).toBeInTheDocument();
  });

  it("adds a table to the selection when its checkbox is checked", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<SchemaTablePicker schemas={schemas} selected={new Set()} onChange={onChange} />);

    await user.click(screen.getByRole("checkbox", { name: "orders" }));

    expect(onChange).toHaveBeenCalledWith(new Set([tableKey("public", "orders")]));
  });

  it("removes a table from the selection when its checkbox is unchecked", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(
      <SchemaTablePicker
        schemas={schemas}
        selected={new Set([tableKey("public", "orders"), tableKey("public", "customers")])}
        onChange={onChange}
      />,
    );

    await user.click(screen.getByRole("checkbox", { name: "orders" }));

    expect(onChange).toHaveBeenCalledWith(new Set([tableKey("public", "customers")]));
  });

  // Direct regression test for the schema-level checkbox's own "select
  // every table in this schema at once" behavior — the whole reason it
  // exists rather than requiring one click per table.
  it("selects every table in a schema when its own checkbox is checked", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<SchemaTablePicker schemas={schemas} selected={new Set()} onChange={onChange} />);

    await user.click(screen.getByRole("checkbox", { name: /select all tables in public/i }));

    expect(onChange).toHaveBeenCalledWith(new Set([tableKey("public", "orders"), tableKey("public", "customers")]));
  });

  it("deselects every table in a schema when its own checkbox is unchecked", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(
      <SchemaTablePicker
        schemas={schemas}
        selected={
          new Set([tableKey("public", "orders"), tableKey("public", "customers"), tableKey("billing", "invoices")])
        }
        onChange={onChange}
      />,
    );

    await user.click(screen.getByRole("checkbox", { name: /select all tables in public/i }));

    expect(onChange).toHaveBeenCalledWith(new Set([tableKey("billing", "invoices")]));
  });

  it("marks the schema checkbox as checked when all of its tables are selected", () => {
    render(
      <SchemaTablePicker
        schemas={schemas}
        selected={new Set([tableKey("public", "orders"), tableKey("public", "customers")])}
        onChange={vi.fn()}
      />,
    );

    expect(screen.getByRole("checkbox", { name: /select all tables in public/i })).toBeChecked();
  });

  // Direct regression test for the indeterminate visual state — a
  // schema with SOME but not all tables selected should show the
  // browser's own tri-state checkbox rendering, not appear fully
  // checked or fully unchecked.
  it("marks the schema checkbox as indeterminate when only some of its tables are selected", () => {
    render(
      <SchemaTablePicker schemas={schemas} selected={new Set([tableKey("public", "orders")])} onChange={vi.fn()} />,
    );

    const schemaCheckbox = screen.getByRole("checkbox", { name: /select all tables in public/i }) as HTMLInputElement;
    expect(schemaCheckbox.indeterminate).toBe(true);
    expect(schemaCheckbox.checked).toBe(false);
  });

  it("shows a placeholder message for a schema with no tables", () => {
    render(<SchemaTablePicker schemas={[{ name: "empty_schema", tables: [] }]} selected={new Set()} onChange={vi.fn()} />);

    expect(screen.getByText(/no tables in this schema/i)).toBeInTheDocument();
  });
});
