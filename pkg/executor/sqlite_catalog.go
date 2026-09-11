package executor

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// SQLite catalog-introspection support for xorm.io/xorm v0.8.0 and
// github.com/glebarez/sqlite v1.11.0. These drivers read the virtual
// sqlite_master / sqlite_schema table and the index_list / index_info /
// table_xinfo pragmas. Rows are synthesized from the durable PizzaSQL schema;
// nothing is persisted.
var sqliteCatalogTables = map[string]bool{
	"sqlite_master": true,
	"sqlite_schema": true,
}

func sqliteCatalogSchema(tableName string) *storage.Schema {
	return &storage.Schema{
		Name: tableName,
		Columns: []storage.Column{
			{Name: "type", Type: "TEXT", Nullable: true},
			{Name: "name", Type: "TEXT", Nullable: true},
			{Name: "tbl_name", Type: "TEXT", Nullable: true},
			{Name: "rootpage", Type: "INTEGER", Nullable: true},
			{Name: "sql", Type: "TEXT", Nullable: true},
		},
	}
}

func isSQLiteCatalogTable(name string) bool {
	return sqliteCatalogTables[strings.ToLower(name)]
}

// sqliteCatalogSelect answers a single-table SELECT against sqlite_master /
// sqlite_schema. handled is false for any other shape so the caller falls
// through to the normal SELECT path.
func (e *Executor) sqliteCatalogSelect(stmt *parser.SelectStmt) (*Result, bool, error) {
	if stmt == nil || stmt.Compound != nil || len(stmt.From) != 1 {
		return nil, false, nil
	}
	ref := stmt.From[0]
	if ref.Subquery != nil || ref.Join != nil || !isSQLiteCatalogTable(ref.Name) {
		return nil, false, nil
	}

	rows, err := e.sqliteCatalogRows()
	if err != nil {
		return nil, true, err
	}
	if ref.Alias != "" {
		for i := range rows {
			rows[i] = e.addTableAlias(rows[i], ref.Alias)
		}
	}

	result, err := e.executeSelectOnRows(stmt, rows, sqliteCatalogSchema(ref.Name))
	if err != nil {
		return nil, true, err
	}
	return result, true, nil
}

// sqliteCatalogRows materializes the sqlite_master row set: one "table" row per
// user table (with recreated CREATE TABLE SQL) and one "index" row per index
// (with recreated CREATE INDEX SQL), deterministically ordered.
func (e *Executor) sqliteCatalogRows() ([]storage.Row, error) {
	var rows []storage.Row

	tables, err := e.schema.ListTables()
	if err != nil {
		return nil, err
	}
	tableNames := append([]string(nil), tables...)
	sort.Strings(tableNames)
	for _, name := range tableNames {
		schema, err := e.schema.GetSchema(name)
		if err != nil {
			return nil, fmt.Errorf("sqlite_catalog: resolve table %q: %w", name, err)
		}
		rows = append(rows, storage.Row{
			"type":     "table",
			"name":     schema.Name,
			"tbl_name": schema.Name,
			"rootpage": int64(0),
			"sql":      recreateCreateTableSQL(schema),
		})
	}

	indexes, err := e.schema.ListIndexes()
	if err != nil {
		return nil, err
	}
	indexNames := append([]string(nil), indexes...)
	sort.Strings(indexNames)
	for _, name := range indexNames {
		idx, err := e.schema.GetIndex(name)
		if err != nil {
			return nil, fmt.Errorf("sqlite_catalog: resolve index %q: %w", name, err)
		}
		rows = append(rows, storage.Row{
			"type":     "index",
			"name":     idx.Name,
			"tbl_name": idx.Table,
			"rootpage": int64(0),
			"sql":      recreateCreateIndexSQL(idx),
		})
	}

	return rows, nil
}

// executeSelectOnRows runs the shared SELECT tail (WHERE, GROUP BY, aggregates,
// ORDER BY/LIMIT/OFFSET, projection, DISTINCT) over an in-memory row set.
func (e *Executor) executeSelectOnRows(stmt *parser.SelectStmt, rows []storage.Row, schema *storage.Schema) (*Result, error) {
	if stmt.Where != nil {
		filtered := make([]storage.Row, 0, len(rows))
		for _, row := range rows {
			val, err := e.evalExpr(stmt.Where, row)
			if err != nil {
				return nil, err
			}
			if toBool(val) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	if len(stmt.GroupBy) > 0 {
		return e.executeGroupBy(stmt, rows, schema)
	}
	if e.hasAggregates(stmt.Columns) {
		return e.executeAggregateSelect(stmt, rows, schema)
	}

	rows = e.orderAndLimitRows(rows, stmt.OrderBy, stmt.Limit, stmt.Offset, stmt.Columns)

	result := NewResult("SELECT")

	for i, col := range stmt.Columns {
		switch {
		case col.Alias != "":
			result.AddColumn(col.Alias)
		case col.Star:
			for _, c := range schema.Columns {
				result.AddColumn(c.Name)
			}
		default:
			if ref, ok := col.Expr.(*parser.ColumnRef); ok {
				result.AddColumn(ref.Column)
			} else {
				result.AddColumn(fmt.Sprintf("column%d", i+1))
			}
		}
	}

	for _, row := range rows {
		values := make([]interface{}, 0, len(stmt.Columns))
		for _, col := range stmt.Columns {
			if col.Star {
				for _, c := range schema.Columns {
					values = append(values, row[c.Name])
				}
			} else {
				val, err := e.evalExpr(col.Expr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, val)
			}
		}
		result.AddRow(values...)
	}

	if stmt.Distinct {
		result.Rows = e.applyDistinct(result.Rows)
	}

	return result, nil
}

// sqliteCatalogPragma answers the introspection pragmas (index_list, index_info,
// table_xinfo). handled is false for everything else so the built-in handler runs.
func (e *Executor) sqliteCatalogPragma(stmt *parser.PragmaStmt) (*Result, bool, error) {
	if stmt == nil {
		return nil, false, nil
	}
	switch strings.ToLower(stmt.Name) {
	case "index_list", "index_info", "table_xinfo":
		res, err := e.executeCatalogPragma(stmt)
		return res, true, err
	default:
		return nil, false, nil
	}
}

func (e *Executor) executeCatalogPragma(stmt *parser.PragmaStmt) (*Result, error) {
	switch strings.ToLower(stmt.Name) {
	case "index_list":
		return e.pragmaIndexList(stmt.Arg)
	case "index_info":
		return e.pragmaIndexInfo(stmt.Arg)
	case "table_xinfo":
		return e.pragmaTableXInfo(stmt.Arg)
	default:
		return nil, fmt.Errorf("unknown pragma: %s", stmt.Name)
	}
}

// pragmaIndexList returns PRAGMA index_list(table): seq, name, unique, origin,
// partial. PizzaSQL only creates indexes via CREATE INDEX, so origin is "c".
func (e *Executor) pragmaIndexList(table string) (*Result, error) {
	if table == "" {
		return nil, fmt.Errorf("index_list requires a table name")
	}
	indexes, err := e.schema.ListTableIndexes(table)
	if err != nil {
		return nil, err
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].Name < indexes[j].Name })

	result := NewResult("PRAGMA")
	for _, c := range []string{"seq", "name", "unique", "origin", "partial"} {
		result.AddColumn(c)
	}
	for i, idx := range indexes {
		unique := int64(0)
		if idx.Unique {
			unique = 1
		}
		result.AddRow(int64(i), idx.Name, unique, "c", int64(0))
	}
	return result, nil
}

// pragmaIndexInfo returns PRAGMA index_info(index): seqno, cid, name.
func (e *Executor) pragmaIndexInfo(name string) (*Result, error) {
	if name == "" {
		return nil, fmt.Errorf("index_info requires an index name")
	}
	idx, err := e.schema.GetIndex(name)
	if err != nil {
		return nil, err
	}
	schema, err := e.schema.GetSchema(idx.Table)
	if err != nil {
		return nil, err
	}

	result := NewResult("PRAGMA")
	for _, c := range []string{"seqno", "cid", "name"} {
		result.AddColumn(c)
	}
	for seqno, ic := range idx.Columns {
		cid := int64(-1)
		for i, c := range schema.Columns {
			if strings.EqualFold(c.Name, ic.Name) {
				cid = int64(i)
				break
			}
		}
		result.AddRow(int64(seqno), cid, ic.Name)
	}
	return result, nil
}

// pragmaTableXInfo returns PRAGMA table_xinfo(table): table_info columns plus a
// trailing hidden flag (always 0).
func (e *Executor) pragmaTableXInfo(table string) (*Result, error) {
	if table == "" {
		return nil, fmt.Errorf("table_xinfo requires a table name")
	}
	schema, err := e.schema.GetSchema(table)
	if err != nil {
		return nil, err
	}

	result := NewResult("PRAGMA")
	for _, c := range []string{"cid", "name", "type", "notnull", "dflt_value", "pk", "hidden"} {
		result.AddColumn(c)
	}
	for i, col := range schema.Columns {
		notnull := int64(0)
		if !col.Nullable {
			notnull = 1
		}
		pk := int64(0)
		if col.PrimaryKey {
			pk = 1
		}
		result.AddRow(int64(i), col.Name, col.Type, notnull, col.Default, pk, int64(0))
	}
	return result, nil
}

// recreateCreateTableSQL rebuilds CREATE TABLE from the durable schema. The
// implicit _rowid_ (and its aliases) are stripped, and identifiers are quoted so
// xorm's IsColumnExist / GORM's HasColumn LIKE patterns match.
func recreateCreateTableSQL(s *storage.Schema) string {
	cols := make([]string, 0, len(s.Columns))
	for _, col := range s.Columns {
		// Strip only the engine-injected hidden rowid column, never user
		// columns that happen to be named oid/rowid/_rowid_.
		if s.PrimaryKey == "_rowid_" && col.Name == "_rowid_" {
			continue
		}
		cols = append(cols, recreateColumnDef(s, col))
	}
	return fmt.Sprintf("CREATE TABLE %s (%s)", quoteIdent(s.Name), strings.Join(cols, ", "))
}

func recreateColumnDef(s *storage.Schema, col storage.Column) string {
	var b strings.Builder
	b.WriteString(quoteIdent(col.Name))
	b.WriteString(" ")
	b.WriteString(col.Type)
	if col.PrimaryKey {
		b.WriteString(" PRIMARY KEY")
	}
	if col.PrimaryKey && s.AutoIncrement {
		b.WriteString(" AUTOINCREMENT")
	}
	if !col.Nullable {
		b.WriteString(" NOT NULL")
	}
	if col.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(sqlLiteral(col.Default))
	}
	return b.String()
}

func recreateCreateIndexSQL(idx *storage.Index) string {
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	cols := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		col := quoteIdent(c.Name)
		if c.Desc {
			col += " DESC"
		}
		cols = append(cols, col)
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", unique, quoteIdent(idx.Name), quoteIdent(idx.Table), strings.Join(cols, ", "))
}

// quoteIdent backtick-quotes an identifier, doubling embedded backticks.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func sqlLiteral(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return "'" + strings.ReplaceAll(fmt.Sprintf("%v", v), "'", "''") + "'"
	}
}
