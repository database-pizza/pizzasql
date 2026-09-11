package executor

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// maxRecursiveCTEIterations bounds fixpoint iteration so a recursive CTE with a
// cycle cannot spin forever.
const maxRecursiveCTEIterations = 1000

// cteTable is a materialized common table expression.
type cteTable struct {
	columns []string
	rows    []storage.Row
}

// cteTableFor returns the materialized CTE named name, if any.
func (e *Executor) cteTableFor(name string) (*cteTable, bool) {
	if e.cteTables == nil {
		return nil, false
	}
	t, ok := e.cteTables[strings.ToLower(name)]
	return t, ok
}

// executeWith materializes every CTE in order, then runs the main query with the
// CTE tables available to table resolution. Nested WITH clauses keep the outer
// tables visible.
func (e *Executor) executeWith(stmt *parser.SelectStmt) (*Result, error) {
	prev := e.cteTables
	next := make(map[string]*cteTable, len(stmt.With)+len(prev))
	for k, v := range prev {
		next[k] = v
	}
	e.cteTables = next
	defer func() { e.cteTables = prev }()

	for _, cte := range stmt.With {
		if err := e.materializeCTE(cte); err != nil {
			return nil, err
		}
	}

	main := *stmt
	main.With = nil
	return e.executeSelect(&main)
}

func (e *Executor) materializeCTE(cte *parser.CTE) error {
	// WITH RECURSIVE marks the whole clause; only a compound CTE with a
	// self-referencing leg is actually recursive. A plain SELECT is just a CTE.
	compound := cte.Query.Compound
	if !cte.Recursive || compound == nil {
		res, err := e.executeSelect(cte.Query)
		if err != nil {
			return err
		}
		e.registerCTEResult(cte.Name, cte.Columns, res)
		return nil
	}

	anchor, err := e.executeSelect(compound.Left)
	if err != nil {
		return err
	}
	cols := cte.Columns
	if len(cols) == 0 {
		cols = anchor.Columns
	}

	accumulated := resultToCTERows(anchor, cols)
	working := accumulated
	distinct := compound.Op != parser.SetOpUnionAll
	seen := make(map[string]bool)
	if distinct {
		for _, row := range accumulated {
			seen[cteRowKey(row, cols)] = true
		}
	}

	for i := 0; i < maxRecursiveCTEIterations; i++ {
		e.cteTables[strings.ToLower(cte.Name)] = &cteTable{columns: cols, rows: working}
		recursive, err := e.executeSelect(compound.Right)
		if err != nil {
			return err
		}
		fresh := make([]storage.Row, 0)
		for _, row := range resultToCTERows(recursive, cols) {
			if distinct {
				key := cteRowKey(row, cols)
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			fresh = append(fresh, row)
		}
		if len(fresh) == 0 {
			break
		}
		accumulated = append(accumulated, fresh...)
		working = fresh
	}

	e.cteTables[strings.ToLower(cte.Name)] = &cteTable{columns: cols, rows: accumulated}
	return nil
}

// registerCTEResult stores a query result as a CTE. Declared column names win;
// otherwise the result's own column names are used.
func (e *Executor) registerCTEResult(name string, declared []string, res *Result) {
	cols := declared
	if len(cols) == 0 {
		cols = res.Columns
	}
	e.cteTables[strings.ToLower(name)] = &cteTable{columns: cols, rows: resultToCTERows(res, cols)}
}

// resultToCTERows maps each result row to a storage.Row keyed by cols
// positionally, which renames a recursive term's columns to the anchor's names.
func resultToCTERows(res *Result, cols []string) []storage.Row {
	rows := make([]storage.Row, 0, len(res.Rows))
	for _, values := range res.Rows {
		row := make(storage.Row, len(cols))
		for i, name := range cols {
			if i < len(values) {
				row[name] = values[i]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// cteRowsToValues converts a materialized CTE to positional row values.
func cteRowsToValues(cte *cteTable) [][]interface{} {
	values := make([][]interface{}, len(cte.rows))
	for i, row := range cte.rows {
		vals := make([]interface{}, len(cte.columns))
		for j, col := range cte.columns {
			vals[j] = row[col]
		}
		values[i] = vals
	}
	return values
}

// cteTableExists reports whether name is a materialized CTE.
func (e *Executor) cteTableExists(name string) bool {
	_, ok := e.cteTableFor(name)
	return ok
}

// materializeJoinSubquery runs a derived table used on the right side of a JOIN
// and returns its rows and schema.
func (e *Executor) materializeJoinSubquery(ref *parser.TableRef) ([]storage.Row, *storage.Schema, error) {
	res, err := e.executeSelect(ref.Subquery)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]storage.Row, 0, len(res.Rows))
	for _, values := range res.Rows {
		row := make(storage.Row, len(res.Columns))
		for i, col := range res.Columns {
			if i < len(values) {
				row[col] = values[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, schemaFromColumns(res.Columns), nil
}

// cloneRows returns a shallow copy of each row so callers cannot mutate a
// materialized CTE's stored rows.
func cloneRows(rows []storage.Row) []storage.Row {
	out := make([]storage.Row, len(rows))
	for i, r := range rows {
		c := make(storage.Row, len(r))
		for k, v := range r {
			c[k] = v
		}
		out[i] = c
	}
	return out
}

func cteRowKey(row storage.Row, cols []string) string {
	var b strings.Builder
	for _, c := range cols {
		fmt.Fprintf(&b, "%v\x00", row[c])
	}
	return b.String()
}
