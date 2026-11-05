package executor

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// foreignKeyDeclaration is one parsed FOREIGN KEY (or inline REFERENCES) before
// it is resolved against the parent table.
type foreignKeyDeclaration struct {
	name       string
	columns    []string
	refTable   string
	refColumns []string
	onDelete   parser.ReferentialAction
	onUpdate   parser.ReferentialAction
	deferrable bool
}

// createTableForeignKeys resolves the FOREIGN KEY declarations of a CREATE TABLE
// into durable constraints. The parent table must already exist with its
// unique key in place, except for a self-reference to the table being created;
// a forward reference fails here instead of being stored and silently ignored.
func (e *Executor) createTableForeignKeys(stmt *parser.CreateTableStmt, schema *storage.Schema, uniqueIndexes []*storage.Index) ([]storage.ForeignKeyConstraint, error) {
	var declarations []foreignKeyDeclaration
	for _, colDef := range stmt.Columns {
		for _, constraint := range colDef.Constraints {
			if constraint.Type != parser.ConstraintForeignKey {
				continue
			}
			declarations = append(declarations, foreignKeyDeclaration{
				name:       constraint.Name,
				columns:    []string{colDef.Name},
				refTable:   constraint.RefTable,
				refColumns: constraint.RefColumns,
				onDelete:   constraint.OnDelete,
				onUpdate:   constraint.OnUpdate,
				deferrable: constraint.Deferrable,
			})
		}
	}
	for _, constraint := range stmt.Constraints {
		if constraint.Type != parser.ConstraintForeignKey {
			continue
		}
		declarations = append(declarations, foreignKeyDeclaration{
			name:       constraint.Name,
			columns:    constraint.Columns,
			refTable:   constraint.RefTable,
			refColumns: constraint.RefColumns,
			onDelete:   constraint.OnDelete,
			onUpdate:   constraint.OnUpdate,
			deferrable: constraint.Deferrable,
		})
	}

	foreignKeys := make([]storage.ForeignKeyConstraint, 0, len(declarations))
	for _, declaration := range declarations {
		foreignKey, err := e.resolveForeignKey(stmt.Table.Name, schema, uniqueIndexes, declaration)
		if err != nil {
			return nil, err
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys, nil
}

// resolveForeignKey validates one declaration and returns its durable form.
func (e *Executor) resolveForeignKey(table string, schema *storage.Schema, uniqueIndexes []*storage.Index, declaration foreignKeyDeclaration) (storage.ForeignKeyConstraint, error) {
	label := declaration.label()
	if declaration.deferrable {
		return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
			"foreign key %s on table %s is DEFERRABLE; foreign keys are always enforced immediately and deferred constraints are not implemented",
			label, table,
		)
	}
	for _, action := range []struct {
		clause string
		value  parser.ReferentialAction
	}{{"ON DELETE", declaration.onDelete}, {"ON UPDATE", declaration.onUpdate}} {
		switch action.value {
		case parser.RefActionCascade, parser.RefActionSetNull, parser.RefActionSetDefault:
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s uses %s %s; cascades are not implemented, use NO ACTION or RESTRICT",
				label, table, action.clause, action.value,
			)
		}
	}

	childColumns := make([]string, 0, len(declaration.columns))
	seen := make(map[string]bool, len(declaration.columns))
	for _, name := range declaration.columns {
		column, ok := schema.GetColumn(name)
		if !ok {
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s names column %q, which does not exist",
				label, table, name,
			)
		}
		if seen[strings.ToLower(column.Name)] {
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s repeats column %q",
				label, table, column.Name,
			)
		}
		seen[strings.ToLower(column.Name)] = true
		childColumns = append(childColumns, column.Name)
	}
	if len(childColumns) == 0 {
		return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
			"foreign key %s on table %s declares no columns", label, table,
		)
	}

	selfReference := strings.EqualFold(declaration.refTable, table)
	var parent *storage.Schema
	var parentIndexes []*storage.Index
	if selfReference {
		parent = schema
		parentIndexes = uniqueIndexes
	} else {
		resolved, err := e.schema.GetSchema(declaration.refTable)
		if err != nil {
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s references table %q, which does not exist; create the parent table before the child table",
				label, table, declaration.refTable,
			)
		}
		parent = resolved
		indexes, err := e.schema.ListTableIndexes(declaration.refTable)
		if err != nil {
			return storage.ForeignKeyConstraint{}, err
		}
		parentIndexes = indexes
	}

	refColumns := append([]string(nil), declaration.refColumns...)
	if len(refColumns) == 0 {
		if parent.UsesSyntheticRowID() {
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s references table %s without a column list, but %s has no PRIMARY KEY; declare a PRIMARY KEY or list parent columns covered by a UNIQUE index",
				label, table, parent.Name, parent.Name,
			)
		}
		refColumns = parent.PrimaryKeyList()
	}
	if len(childColumns) != len(refColumns) {
		return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
			"foreign key %s on table %s has %d child column(s) but references %d parent column(s); both sides of a foreign key need the same number of columns",
			label, table, len(childColumns), len(refColumns),
		)
	}

	canonical := make([]string, 0, len(refColumns))
	for _, name := range refColumns {
		column, ok := parent.GetColumn(name)
		if !ok {
			return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
				"foreign key %s on table %s references column %q, which does not exist in table %s",
				label, table, name, parent.Name,
			)
		}
		canonical = append(canonical, column.Name)
	}

	if !foreignKeyTargetIsUnique(parent, canonical, parentIndexes) {
		return storage.ForeignKeyConstraint{}, pgerror.Unsupported(
			"foreign key mismatch: table %s references %s(%s), but the referenced columns are neither the parent's PRIMARY KEY nor covered by a UNIQUE index; declare UNIQUE (%s) on %s first",
			table, parent.Name, strings.Join(canonical, ", "), strings.Join(canonical, ", "), parent.Name,
		)
	}

	return storage.ForeignKeyConstraint{
		Name:       declaration.name,
		Columns:    childColumns,
		RefTable:   parent.Name,
		RefColumns: canonical,
		OnDelete:   declaration.onDelete.String(),
		OnUpdate:   declaration.onUpdate.String(),
	}, nil
}

// label names the declaration in a DDL error before it has been resolved.
func (d foreignKeyDeclaration) label() string {
	if d.name != "" {
		return d.name
	}
	if len(d.columns) == 0 {
		return "FOREIGN KEY"
	}
	return fmt.Sprintf("FOREIGN KEY (%s)", strings.Join(d.columns, ", "))
}

// foreignKeyTargetIsUnique reports whether columns is exactly the parent's
// primary key or exactly the column set of a UNIQUE index on the parent.
func foreignKeyTargetIsUnique(parent *storage.Schema, columns []string, indexes []*storage.Index) bool {
	if !parent.UsesSyntheticRowID() && parent.PrimaryKeyTargetMatches(columns) {
		return true
	}
	for _, index := range indexes {
		if !index.Unique || len(index.Columns) != len(columns) {
			continue
		}
		plain := true
		names := make([]string, len(index.Columns))
		for i, column := range index.Columns {
			if column.Expression != "" {
				plain = false
				break
			}
			names[i] = column.Name
		}
		if plain && sameColumnSet(names, columns) {
			return true
		}
	}
	return false
}

// sameColumnSet reports whether both lists name the same columns, in any order.
func sameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, name := range a {
		found := false
		for _, other := range b {
			if strings.EqualFold(name, other) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// rejectForeignKeyColumnChange refuses to drop or rename a column that a
// persisted foreign key depends on, on the child side or as a referenced parent
// column. Constraint metadata stores column names verbatim and cannot be
// rewritten, so accepting the change would either silently stop enforcing the
// constraint or leave the catalog naming a missing column.
func (e *Executor) rejectForeignKeyColumnChange(table, column, action string) error {
	labels, err := e.schema.ForeignKeyColumnDependency(table, column)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return nil
	}
	return pgerror.Unsupported(
		"cannot %s column %s: foreign key %s depends on it; drop the constrained table (or recreate it without that constraint) first",
		action, column, strings.Join(labels, ", "),
	)
}

// rejectForeignKeyTableRename refuses to rename a table another constraint (or
// its own constraint) names as the parent. The referenced table name is
// persisted verbatim, so a rename would leave parent-side enforcement pointing
// at a table that no longer exists.
func (e *Executor) rejectForeignKeyTableRename(table string) error {
	labels, err := e.schema.ForeignKeyTableDependency(table)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return nil
	}
	return pgerror.Unsupported(
		"cannot rename table %s: foreign key %s references it; drop the constrained table (or recreate it without that constraint) first",
		table, strings.Join(labels, ", "),
	)
}

// rejectForeignKeyDependentTableDrop refuses to drop a table that a persisted
// foreign key names as its parent, because the constraint would be left dangling
// on the child table.
func (e *Executor) rejectForeignKeyDependentTableDrop(table string) error {
	labels, err := e.schema.ForeignKeyTableDependency(table)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return nil
	}
	return pgerror.Unsupported(
		"cannot drop table %s: foreign key %s references it; drop or recreate the table that declares that constraint first",
		table, strings.Join(labels, ", "),
	)
}
