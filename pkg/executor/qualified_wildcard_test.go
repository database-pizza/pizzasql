package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// startPizzaKV launches a local PizzaKV for a test and returns a pool connected
// to it plus a cleanup func. Skips when PIZZAKV_BIN is not set.
func startPizzaKV(t *testing.T) (*storage.KVPool, func()) {
	t.Helper()
	binary := os.Getenv("PIZZAKV_BIN")
	if binary == "" {
		t.Skip("PIZZAKV_BIN is not set")
	}

	dir := t.TempDir()
	socket := filepath.Join(dir, "kv.sock")
	database := filepath.Join(dir, "test.pkvdb")

	cmd := exec.Command(binary, "-unix="+socket, "-path="+database)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start PizzaKV: %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)

	addr := "unix:" + socket
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pool, err := storage.NewKVPool(addr, 2, 5*time.Second)
		if err == nil {
			return pool, stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PizzaKV did not become ready")
	return nil, nil
}

func TestQualifiedWildcardProjection(t *testing.T) {
	pool, stop := startPizzaKV(t)
	if pool == nil {
		return
	}
	defer stop()
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, "gogs")
	table := storage.NewTableManager(pool, schema, "gogs")
	exec := New(schema, table)

	mustExec := func(sql string) *Result {
		t.Helper()
		res, err := execSQL(exec, sql)
		if err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		return res
	}

	mustExec(`CREATE TABLE repository (id INTEGER PRIMARY KEY, owner_id INTEGER, name TEXT)`)
	mustExec(`CREATE TABLE access (id INTEGER PRIMARY KEY, user_id INTEGER, repo_id INTEGER, mode INTEGER)`)

	mustExec(`INSERT INTO repository VALUES (1, 100, 'gogs')`)
	mustExec(`INSERT INTO repository VALUES (2, 100, 'pizza')`)
	mustExec(`INSERT INTO repository VALUES (3, 200, 'shared')`)

	mustExec(`INSERT INTO access VALUES (1, 1, 1, 2)`)
	mustExec(`INSERT INTO access VALUES (2, 1, 2, 1)`)
	mustExec(`INSERT INTO access VALUES (3, 2, 1, 1)`)

	t.Run("single_table_alias", func(t *testing.T) {
		res := mustExec(`SELECT repo.* FROM repository AS repo ORDER BY repo.id`)
		wantCols := []string{"id", "owner_id", "name"}
		if !reflect.DeepEqual(res.Columns, wantCols) {
			t.Fatalf("columns = %v, want %v", res.Columns, wantCols)
		}
		if res.RowCount != 3 {
			t.Fatalf("row count = %d, want 3", res.RowCount)
		}
		if res.Rows[0][0] != int64(1) || res.Rows[0][1] != int64(100) || res.Rows[0][2] != "gogs" {
			t.Fatalf("row 0 = %v", res.Rows[0])
		}
	})

	t.Run("distinct_left_join_only_repo", func(t *testing.T) {
		// The Gogs SearchRepositoryByName shape: qualified wildcard on the left
		// table of a LEFT JOIN must not leak joined-table columns.
		res := mustExec(`SELECT DISTINCT repo.* FROM repository AS repo LEFT JOIN access ON access.repo_id = repo.id WHERE repo.owner_id = 100 ORDER BY repo.id`)
		wantCols := []string{"id", "owner_id", "name"}
		if !reflect.DeepEqual(res.Columns, wantCols) {
			t.Fatalf("columns = %v, want %v", res.Columns, wantCols)
		}
		if res.RowCount != 2 {
			t.Fatalf("row count = %d, want 2 (DISTINCT dedupe)", res.RowCount)
		}
		if res.Rows[0][0] != int64(1) || res.Rows[0][2] != "gogs" {
			t.Fatalf("row 0 = %v", res.Rows[0])
		}
		if res.Rows[1][0] != int64(2) || res.Rows[1][2] != "pizza" {
			t.Fatalf("row 1 = %v", res.Rows[1])
		}
	})

	t.Run("mixed_wildcard_and_qualified_column", func(t *testing.T) {
		res := mustExec(`SELECT repo.*, access.mode FROM repository AS repo LEFT JOIN access ON access.repo_id = repo.id WHERE repo.id = 1 ORDER BY access.mode`)
		wantCols := []string{"id", "owner_id", "name", "mode"}
		if !reflect.DeepEqual(res.Columns, wantCols) {
			t.Fatalf("columns = %v, want %v", res.Columns, wantCols)
		}
		if res.RowCount != 2 {
			t.Fatalf("row count = %d, want 2", res.RowCount)
		}
		if res.Rows[0][3] != int64(1) || res.Rows[1][3] != int64(2) {
			t.Fatalf("modes = %v, want [1 2]", [][]interface{}{res.Rows[0], res.Rows[1]})
		}
	})

	t.Run("unknown_qualifier_errors", func(t *testing.T) {
		_, err := execSQL(exec, `SELECT nope.* FROM repository AS repo`)
		if err == nil {
			t.Fatal("expected error for unknown wildcard qualifier, got nil")
		}
	})

	t.Run("unaliased_table_wildcard", func(t *testing.T) {
		res := mustExec(`SELECT repository.* FROM repository ORDER BY id`)
		wantCols := []string{"id", "owner_id", "name"}
		if !reflect.DeepEqual(res.Columns, wantCols) {
			t.Fatalf("columns = %v, want %v", res.Columns, wantCols)
		}
		wantTypes := []string{"INTEGER", "INTEGER", "TEXT"}
		if !reflect.DeepEqual(res.ColumnTypes, wantTypes) {
			t.Fatalf("column types = %v, want %v", res.ColumnTypes, wantTypes)
		}
	})
}
