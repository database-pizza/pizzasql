package executor

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func TestBoundedScanAndConjunctionLookup(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(8)
	defer pool.Close()
	schema := storage.NewSchemaManager(pool, "bounded")
	table := storage.NewTableManager(pool, schema, "bounded")
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)")
	var rows []string
	for i := 1; i <= 2048; i++ {
		rows = append(rows, fmt.Sprintf("(%d,%d)", i, i))
	}
	execMust(t, e, "INSERT INTO t VALUES "+strings.Join(rows, ","))
	full := execMust(t, e, "SELECT id FROM t WHERE v>=0").Rows
	for _, tc := range []struct {
		sql   string
		want  [][]interface{}
		pages int
	}{
		{"SELECT id FROM t LIMIT 3", full[:3], 1},
		{"SELECT id FROM t WHERE v>=0 LIMIT 3 OFFSET 2", full[2:5], 1},
		{"SELECT id FROM t LIMIT 0", nil, 0},
		{"SELECT id FROM t WHERE v>=0 AND id=1000", [][]interface{}{{int64(1000)}}, 0},
		{"SELECT id FROM t WHERE id=1000 AND v<0", nil, 0},
	} {
		before := kv.RequestCount(10) // PKBFI SCAN_NEXT
		got := execMust(t, e, tc.sql).Rows
		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Fatalf("%s: got %v want %v", tc.sql, got, tc.want)
		}
		if pages := kv.RequestCount(10) - before; pages != tc.pages {
			t.Fatalf("%s: scanned %d pages want %d", tc.sql, pages, tc.pages)
		}
	}
	execMust(t, e, "CREATE INDEX t_v ON t(v)")
	before := kv.RequestCount(10)
	if got := execMust(t, e, "SELECT id FROM t WHERE id>0 AND v=1000").Rows; !reflect.DeepEqual(got, [][]interface{}{{int64(1000)}}) {
		t.Fatal(got)
	}
	if kv.RequestCount(10) != before {
		t.Fatal("indexed conjunct fell back to scan")
	}
	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO t VALUES (9999,9999)")
	if got := execMust(t, e, "SELECT id FROM t WHERE v>9000 LIMIT 1").Rows; !reflect.DeepEqual(got, [][]interface{}{{int64(9999)}}) {
		t.Fatalf("bounded scan lost transaction overlay: %v", got)
	}
	execMust(t, e, "SELECT id FROM t LIMIT 1")
	other := newExec(schema, table)
	execMust(t, other, "UPDATE t SET v=0 WHERE id=2048")
	if _, err := execSQL(e, "COMMIT"); !errors.Is(err, storage.ErrSerialization) {
		t.Fatalf("limited transaction lost scan validation: %v", err)
	}
}

func TestScanLimitRejectsUnsafeShapes(t *testing.T) {
	for _, sql := range []string{
		"SELECT DISTINCT v FROM t LIMIT 1",
		"SELECT COUNT(*) FROM t LIMIT 1",
		"SELECT v FROM t GROUP BY v LIMIT 1",
		"SELECT v FROM t ORDER BY v LIMIT 1",
		"SELECT ROW_NUMBER() OVER (ORDER BY v) FROM t LIMIT 1",
		"SELECT (SELECT 1) FROM t LIMIT 1",
		"SELECT v FROM t LIMIT -1",
		"SELECT v FROM t LIMIT 1+1",
	} {
		stmt := parse(t, sql)
		// Parsing is enough: eligibility must not execute expressions or scan data.
		if _, ok := scanLimit(stmt.(*parser.SelectStmt)); ok {
			t.Fatalf("unsafe bound for %s", sql)
		}
	}
}
