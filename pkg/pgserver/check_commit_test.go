package pgserver

import (
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func simpleQueryMessage(sql string) *Message {
	return &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}
}

// A row constraint that only the commit can detect (a column and its CHECK were
// published after the row was staged) must report its SQLSTATE, leave the wire
// session idle, and let the client's cleanup ROLLBACK complete.
func TestConstraintViolationAtCommitClearsTransactionState(t *testing.T) {
	tests := []struct {
		name string
		ddl  string
		code string
	}{
		{
			name: "check",
			ddl:  "ALTER TABLE t ADD COLUMN qty INTEGER DEFAULT -1 CHECK (qty >= 0)",
			code: ErrCodeCheckViolation,
		},
		{
			name: "not null",
			ddl:  "ALTER TABLE t ADD COLUMN note TEXT NOT NULL",
			code: "23502",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := testkv.New(t)
			pool := kv.Pool(8)
			t.Cleanup(func() { pool.Close() })
			schema := storage.NewSchemaManager(pool, "pg_commit_constraints")
			table := storage.NewTableManager(pool, schema, "pg_commit_constraints")

			writer, writerClient := newDataConnectionFor(t, schema, table)
			ddl, ddlClient := newDataConnectionFor(t, schema, table)

			runQuery(t, writer, writerClient, simpleQueryMessage("CREATE TABLE t (id INTEGER PRIMARY KEY)"))
			runQuery(t, writer, writerClient, simpleQueryMessage("BEGIN"))
			runQuery(t, writer, writerClient, simpleQueryMessage("INSERT INTO t (id) VALUES (1)"))

			// The row is only staged, so the new constraint sees an empty table
			// and the commit is where the staged row violates it.
			runQuery(t, ddl, ddlClient, simpleQueryMessage(tt.ddl))

			msgs := runQuery(t, writer, writerClient, simpleQueryMessage("COMMIT"))
			if code := errorCode(msgs[0]); code != tt.code {
				t.Fatalf("COMMIT SQLSTATE = %q, want %q", code, tt.code)
			}
			if writer.txStatus != TxStatusIdle {
				t.Fatalf("tx status = %v, want idle after a failed COMMIT", writer.txStatus)
			}

			// Storage already rolled the transaction back, so the client's
			// cleanup ROLLBACK is a no-op rather than an error.
			msgs = runQuery(t, writer, writerClient, simpleQueryMessage("ROLLBACK"))
			if len(msgs) == 0 || msgs[0].Type == MsgErrorResponse {
				t.Fatalf("ROLLBACK after failed COMMIT = %v, want a successful no-op", msgs)
			}
			if writer.txStatus != TxStatusIdle {
				t.Fatalf("tx status = %v, want idle after the no-op ROLLBACK", writer.txStatus)
			}

			// The session keeps working.
			msgs = runQuery(t, writer, writerClient, simpleQueryMessage("SELECT 1"))
			if len(msgs) < 2 || msgs[0].Type != MsgRowDescription {
				t.Fatalf("SELECT after failed COMMIT = %v, want a result", msgs)
			}
		})
	}
}
