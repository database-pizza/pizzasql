package pgserver

import (
	"testing"
)

// A dangling foreign key reports SQLSTATE 23503 on both the simple and the
// extended protocol, and inside a transaction block it leaves the transaction
// failed like the other integrity errors.
func TestForeignKeySQLStateAcrossProtocolPaths(t *testing.T) {
	for _, extended := range []bool{false, true} {
		for _, transaction := range []bool{false, true} {
			c, client := newDataConnection(t)
			runQuery(t, c, client, simpleQueryMessage("CREATE TABLE parent (id INTEGER PRIMARY KEY)"))
			runQuery(t, c, client, simpleQueryMessage("CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))"))
			if transaction {
				runQuery(t, c, client, simpleQueryMessage("BEGIN"))
			}

			sql := "INSERT INTO child (id, parent_id) VALUES (1, 99)"
			var messages []*Message
			if extended {
				messages = runExtendedExecution(t, c, client, sql)
			} else {
				messages = runQuery(t, c, client, simpleQueryMessage(sql))
			}
			if transaction {
				// A staged write is validated at COMMIT, not by the INSERT.
				if code := errorCode(messages[0]); code != "" {
					t.Fatalf("extended=%v staged INSERT SQLSTATE = %q, want none", extended, code)
				}
				messages = runQuery(t, c, client, simpleQueryMessage("COMMIT"))
			}
			if code := errorCode(messages[0]); code != ErrCodeForeignKeyViolation {
				t.Fatalf("extended=%v transaction=%v SQLSTATE = %q, want %q", extended, transaction, code, ErrCodeForeignKeyViolation)
			}
			if transaction {
				if c.txStatus != TxStatusIdle {
					t.Fatalf("tx status = %v, want idle after a failed COMMIT", c.txStatus)
				}
				runQuery(t, c, client, simpleQueryMessage("ROLLBACK"))
			}

			count := runQuery(t, c, client, simpleQueryMessage("SELECT COUNT(*) FROM child"))
			if values := dataRowValues(t, count[1]); len(values) != 1 || values[0] != "0" {
				t.Fatalf("rejected insert left rows behind: %v", values)
			}
		}
	}
}

// Deleting a referenced parent reports 23503 over the wire and leaves the
// parent row in place.
func TestForeignKeyParentDeleteSQLState(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, simpleQueryMessage("CREATE TABLE parent (id INTEGER PRIMARY KEY)"))
	runQuery(t, c, client, simpleQueryMessage("CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))"))
	runQuery(t, c, client, simpleQueryMessage("INSERT INTO parent (id) VALUES (1)"))
	runQuery(t, c, client, simpleQueryMessage("INSERT INTO child (id, parent_id) VALUES (10, 1)"))

	messages := runQuery(t, c, client, simpleQueryMessage("DELETE FROM parent WHERE id = 1"))
	if code := errorCode(messages[0]); code != ErrCodeForeignKeyViolation {
		t.Fatalf("DELETE SQLSTATE = %q, want %q", code, ErrCodeForeignKeyViolation)
	}
	count := runQuery(t, c, client, simpleQueryMessage("SELECT COUNT(*) FROM parent"))
	if values := dataRowValues(t, count[1]); len(values) != 1 || values[0] != "1" {
		t.Fatalf("rejected delete removed the parent: %v", values)
	}
}
