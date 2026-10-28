package executor

import (
	"strings"
	"testing"
)

func TestCreateTablePrimaryKeyConstraints(t *testing.T) {
	tests := []struct {
		name           string
		sql            string
		wantErr        string
		wantPrimaryKey string
	}{
		{
			name:    "composite table-level primary key is rejected",
			sql:     "CREATE TABLE t (a TEXT, b TEXT, PRIMARY KEY (a, b))",
			wantErr: "composite primary key",
		},
		{
			name:    "duplicate column composite primary key is rejected",
			sql:     "CREATE TABLE t (a TEXT, PRIMARY KEY (a, a))",
			wantErr: "composite primary key",
		},
		{
			name:           "single-column table-level primary key is accepted",
			sql:            "CREATE TABLE t (a TEXT, PRIMARY KEY (a))",
			wantPrimaryKey: "a",
		},
		{
			name:           "inline primary key is accepted",
			sql:            "CREATE TABLE t (a TEXT PRIMARY KEY)",
			wantPrimaryKey: "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, schema, table := newTestDB(t)
			e := newExec(schema, table)

			_, err := execSQL(e, tt.sql)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected %q to fail", tt.sql)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				if schema.TableExists("t") {
					t.Fatal("rejected CREATE TABLE left a schema behind")
				}
				if _, err := execSQL(e, "SELECT * FROM t"); err == nil {
					t.Fatal("rejected CREATE TABLE left a usable table behind")
				}
				return
			}

			if err != nil {
				t.Fatalf("exec %q: %v", tt.sql, err)
			}
			got, err := schema.GetSchema("t")
			if err != nil {
				t.Fatalf("get schema: %v", err)
			}
			if got.PrimaryKey != tt.wantPrimaryKey {
				t.Fatalf("PrimaryKey = %q, want %q", got.PrimaryKey, tt.wantPrimaryKey)
			}
			execMust(t, e, "INSERT INTO t (a) VALUES ('x')")
		})
	}
}
