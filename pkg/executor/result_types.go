package executor

import (
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// selectColumnTypes computes the result column types for a single-table SELECT
// projection, parallel to the column names produced by the normal projection
// expansion. Direct column references (including aliases) and SELECT * resolve
// to the declared schema type; any expression whose type the engine does not
// know is reported as TEXT. Types are derived from schema metadata rather than
// row values so they remain correct even when the result set is empty.
func selectColumnTypes(stmt *parser.SelectStmt, schema *storage.Schema) []string {
	types := make([]string, 0, len(stmt.Columns))
	for _, col := range stmt.Columns {
		if col.Star {
			for _, c := range schema.Columns {
				types = append(types, c.Type)
			}
			continue
		}
		if col.TableStar != "" {
			// Single-table qualified wildcard: expand the table's columns. The
			// analyzer already validated the qualifier, and for this path the
			// FROM clause holds exactly one table.
			for _, c := range schema.Columns {
				types = append(types, c.Type)
			}
			continue
		}
		types = append(types, projectionColumnType(col, schema))
	}
	return types
}

// projectionColumnType resolves the type of a single projected expression. A
// direct column reference resolves to the declared schema type (matched
// case-insensitively and ignoring any table qualifier); anything else is
// unknown and reported as TEXT.
func projectionColumnType(col parser.SelectColumn, schema *storage.Schema) string {
	ref, ok := col.Expr.(*parser.ColumnRef)
	if !ok {
		return "TEXT"
	}
	for _, c := range schema.Columns {
		if strings.EqualFold(c.Name, ref.Column) {
			return c.Type
		}
	}
	return "TEXT"
}

// joinedSelectColumnTypes resolves direct projections against every table in a
// joined or comma-separated FROM clause. Expressions retain TEXT metadata.
func (e *Executor) joinedSelectColumnTypes(stmt *parser.SelectStmt, refs []parser.TableRef) []string {
	types := make([]string, 0, len(stmt.Columns))
	for _, projection := range stmt.Columns {
		if projection.Star {
			for _, ref := range refs {
				if schema, err := e.schema.GetSchema(ref.Name); err == nil {
					for _, column := range schema.Columns {
						types = append(types, column.Type)
					}
				}
			}
			continue
		}
		if projection.TableStar != "" {
			if columns, _, err := e.resolveTableStar(stmt.From, projection.TableStar); err == nil {
				for _, column := range columns {
					types = append(types, column.Type)
				}
			}
			continue
		}

		ref, ok := projection.Expr.(*parser.ColumnRef)
		if !ok {
			types = append(types, "TEXT")
			continue
		}
		columnType := "TEXT"
		found := false
		for _, table := range refs {
			if ref.Table != "" && !strings.EqualFold(ref.Table, table.Alias) && !strings.EqualFold(ref.Table, table.Name) {
				continue
			}
			schema, err := e.schema.GetSchema(table.Name)
			if err != nil {
				continue
			}
			for _, column := range schema.Columns {
				if strings.EqualFold(column.Name, ref.Column) {
					columnType = column.Type
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		types = append(types, columnType)
	}
	return types
}
