package executor

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// returningProjection expands a RETURNING column list into output column names
// and types. A wildcard expands the table's schema columns; an aliased
// expression uses its alias; a bare column reference uses the column name; any
// other expression falls back to a synthesized name.
func returningProjection(cols []parser.SelectColumn, schema *storage.Schema) ([]string, []string) {
	var names, types []string
	for i, col := range cols {
		switch {
		case col.Star:
			for _, c := range schema.Columns {
				names = append(names, c.Name)
				types = append(types, c.Type)
			}
		case col.TableStar != "":
			for _, c := range schema.Columns {
				names = append(names, c.Name)
				types = append(types, c.Type)
			}
		case col.Alias != "":
			names = append(names, col.Alias)
			types = append(types, projectionType(col.Expr, schema))
		default:
			if ref, ok := col.Expr.(*parser.ColumnRef); ok {
				names = append(names, ref.Column)
			} else {
				name := parser.FormatExpr(col.Expr)
				if name == "" {
					name = fmt.Sprintf("column%d", i+1)
				}
				names = append(names, name)
			}
			types = append(types, projectionType(col.Expr, schema))
		}
	}
	return names, types
}

// projectionType resolves the declared type of a direct column reference.
func projectionType(expr parser.Expr, schema *storage.Schema) string {
	ref, ok := expr.(*parser.ColumnRef)
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

// returningResult evaluates a RETURNING projection over the affected rows and
// builds the result set. For INSERT/UPDATE the rows are the post-change rows;
// for DELETE the caller passes the removed rows.
func (e *Executor) returningResult(cols []parser.SelectColumn, schema *storage.Schema, rows []storage.Row) (*Result, error) {
	names, types := returningProjection(cols, schema)
	result := NewResult("SELECT")
	for i := range names {
		result.AddColumnWithType(names[i], types[i])
	}

	for _, row := range rows {
		values := make([]interface{}, 0, len(cols))
		for _, col := range cols {
			switch {
			case col.Star:
				for _, c := range schema.Columns {
					values = append(values, e.lookupRowColumn(row, c.Name))
				}
			case col.TableStar != "":
				for _, c := range schema.Columns {
					values = append(values, e.lookupRowColumn(row, c.Name))
				}
			default:
				val, err := e.evalExpr(col.Expr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, val)
			}
		}
		result.AddRow(values...)
	}
	return result, nil
}

// lookupRowColumn resolves a column from a row case-insensitively.
func (e *Executor) lookupRowColumn(row storage.Row, name string) interface{} {
	if v, ok := row[name]; ok {
		return v
	}
	for k, v := range row {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}
