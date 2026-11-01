package executor

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func TestNotNullFinalRowValidation(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER NOT NULL DEFAULT 7, w INTEGER NOT NULL)")
	execMust(t, e, "INSERT INTO t (id,w) VALUES (1,2)")
	for _, explicit := range []bool{false, true} {
		if explicit {
			execMust(t, e, "BEGIN")
		}
		for _, sql := range []string{
			"INSERT INTO t VALUES (2,NULL,3)",
			"INSERT INTO t (id) VALUES (2)",
			"INSERT INTO t VALUES (2,7,3),(3,7,NULL)",
			"UPDATE t SET v=NULL WHERE id=1",
			"UPDATE t SET w=NULL",
			"INSERT INTO t VALUES (1,7,4) ON CONFLICT (id) DO UPDATE SET w=NULL",
		} {
			_, err := execSQL(e, sql)
			var violation *storage.NotNullViolationError
			if !errors.As(err, &violation) {
				t.Fatalf("tx=%v %s: expected typed NOT NULL violation, got %v", explicit, sql, err)
			}
			rows := execMust(t, e, "SELECT id,v,w FROM t").Rows
			if len(rows) != 1 || rows[0][1] != int64(7) || rows[0][2] != int64(2) {
				t.Fatalf("failed mutation changed rows: %v", rows)
			}
		}
		if explicit {
			execMust(t, e, "COMMIT")
		}
	}
	if _, err := table.InsertBulk("t", []storage.Row{{"id": int64(2), "v": nil, "w": int64(3)}}); err == nil {
		t.Fatal("bulk insert accepted explicit NULL")
	}
}
