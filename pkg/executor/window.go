package executor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// computeWindowValues evaluates each window function in a projection over the
// given rows, returning one value per row for each window expression.
func (e *Executor) computeWindowValues(stmt *parser.SelectStmt, rows []storage.Row) (map[*parser.WindowExpr][]interface{}, error) {
	var windows []*parser.WindowExpr
	for _, col := range stmt.Columns {
		if we, ok := col.Expr.(*parser.WindowExpr); ok {
			windows = append(windows, we)
		}
	}
	if len(windows) == 0 {
		return nil, nil
	}

	out := make(map[*parser.WindowExpr][]interface{}, len(windows))
	for _, we := range windows {
		values, err := e.evalWindowExpr(we, rows)
		if err != nil {
			return nil, err
		}
		out[we] = values
	}
	return out, nil
}

type windowRow struct {
	idx int
	key []interface{}
}

// evalWindowExpr computes ROW_NUMBER() OVER (PARTITION BY ... ORDER BY ...).
// Other window functions are rejected rather than approximated.
func (e *Executor) evalWindowExpr(we *parser.WindowExpr, rows []storage.Row) ([]interface{}, error) {
	name := ""
	if we.Func != nil {
		name = strings.ToUpper(we.Func.Name)
	}
	if name != "ROW_NUMBER" {
		return nil, fmt.Errorf("unsupported window function: %s", name)
	}

	partitions := map[string][]int{}
	var order []string
	for i, row := range rows {
		key, err := e.windowPartitionKey(we, row)
		if err != nil {
			return nil, err
		}
		if _, ok := partitions[key]; !ok {
			order = append(order, key)
		}
		partitions[key] = append(partitions[key], i)
	}

	values := make([]interface{}, len(rows))
	for _, key := range order {
		partition := make([]windowRow, 0, len(partitions[key]))
		for _, idx := range partitions[key] {
			wr := windowRow{idx: idx}
			if len(we.OrderBy) > 0 {
				wr.key = make([]interface{}, len(we.OrderBy))
				for k, item := range we.OrderBy {
					wr.key[k], _ = e.evalExpr(item.Expr, rows[idx])
				}
			}
			partition = append(partition, wr)
		}
		if len(we.OrderBy) > 0 {
			sort.SliceStable(partition, func(a, b int) bool {
				return orderByLess(partition[a].key, partition[b].key, we.OrderBy)
			})
		}
		for rank, wr := range partition {
			values[wr.idx] = int64(rank + 1)
		}
	}
	return values, nil
}

func (e *Executor) windowPartitionKey(we *parser.WindowExpr, row storage.Row) (string, error) {
	if len(we.PartitionBy) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, p := range we.PartitionBy {
		v, err := e.evalExpr(p, row)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%v\x00", v)
	}
	return b.String(), nil
}
