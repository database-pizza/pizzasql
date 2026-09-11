package executor

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// generatedColumnExpr resolves a column's generated expression, if any.
func (e *Executor) generatedColumnExpr(col storage.Column) (parser.Expr, bool, error) {
	if col.GeneratedExpr == "" {
		return nil, false, nil
	}
	expr, err := parseStoredExpr(col.GeneratedExpr)
	if err != nil {
		return nil, false, err
	}
	return expr, true, nil
}

// applyGeneratedColumns recomputes and stores every generated column value on
// row. It runs after the base columns of an INSERT/UPDATE have been resolved so
// STORED generated values are materialized in the durable row.
func (e *Executor) applyGeneratedColumns(schema *storage.Schema, row storage.Row) error {
	for _, col := range schema.Columns {
		expr, ok, err := e.generatedColumnExpr(col)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		val, err := e.evalExpr(expr, row)
		if err != nil {
			return fmt.Errorf("evaluating generated column %s: %w", col.Name, err)
		}
		row[col.Name] = val
	}
	normalizeStoredValues(row)
	return nil
}

// normalizeStoredValues converts executor-internal value types (currently the
// JSON1 subtype) into the plain scalar types the storage codec persists.
func normalizeStoredValues(row storage.Row) {
	for k, v := range row {
		if jt, ok := v.(jsonText); ok {
			row[k] = string(jt)
		}
	}
}

// generatedColumnSet returns the lowercased names of generated columns.
func generatedColumnSet(schema *storage.Schema) map[string]bool {
	set := make(map[string]bool)
	for _, col := range schema.Columns {
		if col.GeneratedExpr != "" {
			set[strings.ToLower(col.Name)] = true
		}
	}
	return set
}

// ensureGeneratedRowID assigns the primary key of a row before generated-column
// evaluation when the key is an engine-generated INTEGER PRIMARY KEY, so a
// generated expression that references the auto-incrementing id (the common
// `stored = id + 1` shape) does not see NULL. Tables without an explicit
// integer primary key are left to the storage layer, which assigns the hidden
// _rowid_ during the insert.
func (e *Executor) ensureGeneratedRowID(tableName string, schema *storage.Schema, row storage.Row) error {
	if schema.PrimaryKey == "" || schema.PrimaryKey == "_rowid_" {
		return nil
	}
	if v, ok := lookupRowValue(row, schema.PrimaryKey); ok && v != nil {
		return nil
	}
	pkCol, ok := schema.GetColumn(schema.PrimaryKey)
	if !ok || !isIntegerColumnType(pkCol.Type) {
		return nil
	}
	id, err := e.schema.GetNextRowID(tableName)
	if err != nil {
		return err
	}
	row[schema.PrimaryKey] = id
	return nil
}

// lookupRowValue resolves a row value case-insensitively.
func lookupRowValue(row storage.Row, name string) (interface{}, bool) {
	if v, ok := row[name]; ok {
		return v, true
	}
	for k, v := range row {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// isIntegerColumnType reports whether a declared type has integer affinity.
func isIntegerColumnType(typeName string) bool {
	return strings.Contains(strings.ToUpper(typeName), "INT")
}

// conflictMatcher describes one uniqueness constraint used to resolve an
// INSERT conflict. index is nil for the primary key.
type conflictMatcher struct {
	name       string
	index      *storage.Index
	primaryKey bool
}

// insertConflictMatchers returns the constraints that should be replaced for an
// INSERT. A statement-level INSERT OR REPLACE replaces on every uniqueness
// constraint; otherwise only indexes declaring ON CONFLICT REPLACE are
// replaced.
func (e *Executor) insertConflictMatchers(tableName string, schema *storage.Schema, stmtReplace bool) ([]conflictMatcher, error) {
	var matchers []conflictMatcher
	if stmtReplace {
		matchers = append(matchers, conflictMatcher{name: schema.PrimaryKey, primaryKey: true})
	}
	indexes, err := e.schema.ListTableIndexes(tableName)
	if err != nil {
		return nil, err
	}
	for _, idx := range indexes {
		if !idx.Unique {
			continue
		}
		if !stmtReplace && !strings.EqualFold(idx.OnConflict, "REPLACE") {
			continue
		}
		matchers = append(matchers, conflictMatcher{name: idx.Name, index: idx})
	}
	return matchers, nil
}

// matcherConflicts returns the durable/overlay rows that the candidate would
// conflict with on a single matcher, using a primary-key point read or an
// index-key lookup rather than a full table scan.
func (e *Executor) matcherConflicts(tableName string, schema *storage.Schema, m conflictMatcher, candidate storage.Row) ([]storage.Row, error) {
	if m.primaryKey {
		pkValue, ok := lookupRowValue(candidate, schema.PrimaryKey)
		if !ok || pkValue == nil {
			return nil, nil
		}
		row, err := e.session.GetByPK(tableName, fmt.Sprintf("%v", pkValue))
		if err == storage.ErrKeyNotFound {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []storage.Row{row}, nil
	}
	isNull, err := e.table.IndexValueContainsNull(m.index, candidate)
	if err != nil {
		return nil, err
	}
	if isNull {
		return nil, nil
	}

	key, err := e.table.IndexRowKey(m.index, candidate)
	if err != nil {
		return nil, err
	}
	return e.session.SelectByIndexKey(tableName, m.index, key)
}

// hasAnyInsertConflict checks every uniqueness constraint. SQLite's
// statement-level OR IGNORE and targetless DO NOTHING apply to any conflict,
// not just the primary key.
func (e *Executor) hasAnyInsertConflict(tableName string, schema *storage.Schema, candidate storage.Row) (bool, error) {
	matchers := []conflictMatcher{{name: schema.PrimaryKey, primaryKey: true}}
	indexes, err := e.schema.ListTableIndexes(tableName)
	if err != nil {
		return false, err
	}
	for _, index := range indexes {
		if index.Unique {
			matchers = append(matchers, conflictMatcher{name: index.Name, index: index})
		}
	}
	for _, matcher := range matchers {
		rows, err := e.matcherConflicts(tableName, schema, matcher, candidate)
		if err != nil {
			return false, err
		}
		if len(rows) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// resolveInsertConflicts removes existing rows that conflict with candidate on
// any constraint that resolves to REPLACE. It runs inside the statement's
// atomic DML block so the deletes and the subsequent insert commit together.
func (e *Executor) resolveInsertConflicts(tableName string, schema *storage.Schema, candidate storage.Row, stmtReplace bool) error {
	matchers, err := e.insertConflictMatchers(tableName, schema, stmtReplace)
	if err != nil {
		return err
	}
	if len(matchers) == 0 {
		return nil
	}

	// Collect matching rows through point/index lookups, then delete them by
	// primary key. A row may match several constraints, so deduplicate.
	toDelete := make(map[string]storage.Row)
	for _, m := range matchers {
		rows, err := e.matcherConflicts(tableName, schema, m, candidate)
		if err != nil {
			return err
		}
		for _, row := range rows {
			toDelete[fmt.Sprintf("%v", row[schema.PrimaryKey])] = row
		}
	}
	if len(toDelete) == 0 {
		return nil
	}

	for _, row := range toDelete {
		if _, deleted, derr := e.session.DeleteByPK(tableName, fmt.Sprintf("%v", row[schema.PrimaryKey])); derr != nil {
			return derr
		} else if !deleted {
			return fmt.Errorf("ON CONFLICT REPLACE row disappeared during delete")
		}
	}
	return nil
}
