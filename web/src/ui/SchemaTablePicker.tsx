import { useEffect, useRef } from "react";
import type { IntrospectedSchema } from "../lib/types";

// tableKey is the canonical "schema.table" identifier this component
// uses internally for its selection Set — matches TableRef's own
// String() method on the Go side (see internal/upgrade.TableRef), kept
// as a single joined string here purely because Set<string> is simpler
// to work with than a Set of object values (which compare by
// reference, not value, in JavaScript).
export function tableKey(schema: string, table: string): string {
  return `${schema}.${table}`;
}

// SchemaTablePicker renders one expandable checkbox group per schema —
// a schema-level checkbox (checked/unchecked/indeterminate depending on
// how many of its own tables are selected) plus one checkbox per table.
// Deliberately emits a flat Set of "schema.table" keys, never "this
// whole schema" as a separate concept — see NewUpgrade.tsx's own
// comment on why the selection is always expanded to an explicit table
// list before submission (matching internal/upgrade.Job.Tables' own
// doc comment on the same design decision, mirrored here).
export function SchemaTablePicker({
  schemas,
  selected,
  onChange,
}: {
  schemas: IntrospectedSchema[];
  selected: Set<string>;
  onChange: (next: Set<string>) => void;
}) {
  function toggleTable(schema: string, table: string) {
    const key = tableKey(schema, table);
    const next = new Set(selected);
    if (next.has(key)) {
      next.delete(key);
    } else {
      next.add(key);
    }
    onChange(next);
  }

  function toggleSchema(schema: IntrospectedSchema, checked: boolean) {
    const next = new Set(selected);
    for (const table of schema.tables) {
      const key = tableKey(schema.name, table);
      if (checked) {
        next.add(key);
      } else {
        next.delete(key);
      }
    }
    onChange(next);
  }

  return (
    <div className="flex flex-col gap-3">
      {schemas.map((schema) => (
        <SchemaGroup
          key={schema.name}
          schema={schema}
          selected={selected}
          onToggleSchema={(checked) => toggleSchema(schema, checked)}
          onToggleTable={(table) => toggleTable(schema.name, table)}
        />
      ))}
    </div>
  );
}

function SchemaGroup({
  schema,
  selected,
  onToggleSchema,
  onToggleTable,
}: {
  schema: IntrospectedSchema;
  selected: Set<string>;
  onToggleSchema: (checked: boolean) => void;
  onToggleTable: (table: string) => void;
}) {
  const selectedCount = schema.tables.filter((t) => selected.has(tableKey(schema.name, t))).length;
  const allSelected = selectedCount === schema.tables.length && schema.tables.length > 0;
  const someSelected = selectedCount > 0 && !allSelected;
  const checkboxRef = useRef<HTMLInputElement>(null);

  // The "select all in this schema" checkbox's indeterminate state
  // isn't a settable JSX prop — it's a DOM property only, so it has to
  // be applied imperatively via a ref, same as any other native
  // checkbox indeterminate state in React.
  useEffect(() => {
    if (checkboxRef.current) {
      checkboxRef.current.indeterminate = someSelected;
    }
  }, [someSelected]);

  return (
    <div className="rounded-lg border border-ink-100 p-3">
      <label className="flex items-center gap-2 text-sm font-medium text-ink-800">
        <input
          ref={checkboxRef}
          type="checkbox"
          checked={allSelected}
          onChange={(e) => onToggleSchema(e.target.checked)}
          aria-label={`Select all tables in ${schema.name}`}
        />
        {schema.name}
        <span className="text-xs font-normal text-ink-400">
          ({selectedCount}/{schema.tables.length} tables)
        </span>
      </label>
      <div className="mt-2 ml-6 flex flex-col gap-1">
        {schema.tables.map((table) => (
          <label key={table} className="flex items-center gap-2 text-sm text-ink-600">
            <input
              type="checkbox"
              checked={selected.has(tableKey(schema.name, table))}
              onChange={() => onToggleTable(table)}
            />
            {table}
          </label>
        ))}
        {schema.tables.length === 0 && <p className="text-xs text-ink-400">No tables in this schema.</p>}
      </div>
    </div>
  );
}
