package pgserver

import (
	"testing"
)

// A CHECK violation reports SQLSTATE 23514 on both the simple and the extended
// protocol and, inside a transaction block, leaves the transaction failed like
// the other integrity errors.
func TestCheckConstraintSQLStateAndTransactionState(t *testing.T) {
	for _, extended := range []bool{false, true} {
		c, client := newDataConnection(t)
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("CREATE TABLE t (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))\x00")})
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("BEGIN\x00")})
		var messages []*Message
		if extended {
			messages = runExtendedExecution(t, c, client, "INSERT INTO t VALUES (1,-1)")
		} else {
			messages = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("INSERT INTO t VALUES (1,-1)\x00")})
		}
		if code := errorCode(messages[0]); code != ErrCodeCheckViolation {
			t.Fatalf("extended=%v SQLSTATE=%q, want %q", extended, code, ErrCodeCheckViolation)
		}
		if c.txStatus != TxStatusFailed {
			t.Fatalf("extended=%v transaction not failed: %c", extended, c.txStatus)
		}
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")})
	}
}
