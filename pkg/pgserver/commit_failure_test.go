package pgserver

import (
	"testing"
)

// A constraint failure at COMMIT is a normal conflict: storage has already
// rolled the transaction back, so the wire session must return to idle instead
// of rejecting every later statement, including ROLLBACK.
func TestFailedCommitClearsTransactionState(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY, email TEXT UNIQUE)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t VALUES (1, 'a@x')"), 0)})

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("BEGIN"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t VALUES (2, 'a@x')"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("COMMIT"), 0)})
	if len(msgs) == 0 || msgs[0].Type != MsgErrorResponse {
		t.Fatalf("COMMIT should fail with the buffered unique violation, got %v", msgs)
	}
	if c.txStatus != TxStatusIdle {
		t.Fatalf("tx status = %v, want idle after a failed COMMIT", c.txStatus)
	}

	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT 1"), 0)})
	if len(msgs) < 2 || msgs[0].Type != MsgRowDescription {
		t.Fatalf("SELECT after failed COMMIT = %v, want a result", msgs)
	}

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("BEGIN"), 0)})
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("ROLLBACK"), 0)})
	if len(msgs) == 0 || msgs[0].Type == MsgErrorResponse {
		t.Fatalf("BEGIN/ROLLBACK after failed COMMIT = %v", msgs)
	}
}
