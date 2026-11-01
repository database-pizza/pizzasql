package pgserver

import (
	"fmt"
	"testing"
)

func TestPrimaryKeySQLStateAcrossMutationAndProtocolPaths(t *testing.T) {
	for _, extended := range []bool{false, true} {
		for _, transaction := range []bool{false, true} {
			for _, sql := range []string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (3),(3)", "UPDATE t SET id=2 WHERE id=1"} {
				t.Run(fmt.Sprintf("extended=%v/transaction=%v/%s", extended, transaction, sql), func(t *testing.T) {
					c, client := newDataConnection(t)
					runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("CREATE TABLE t (id INTEGER PRIMARY KEY)\x00")})
					runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("INSERT INTO t VALUES (1),(2)\x00")})
					if transaction {
						runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("BEGIN\x00")})
					}
					var messages []*Message
					if extended {
						messages = runExtendedExecution(t, c, client, sql)
					} else {
						messages = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte(sql + "\x00")})
					}
					if code := errorCode(messages[0]); code != ErrCodeUniqueViolation {
						t.Fatalf("SQLSTATE=%s want 23505", code)
					}
					if transaction {
						if c.txStatus != TxStatusFailed {
							t.Fatalf("transaction status %c want E", c.txStatus)
						}
						blocked := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT 1\x00")})
						if code := errorCode(blocked[0]); code != ErrCodeTransactionAborted {
							t.Fatalf("aborted SQLSTATE=%s", code)
						}
						runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")})
					}
					count := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT COUNT(*) FROM t\x00")})
					if values := dataRowValues(t, count[1]); len(values) != 1 || values[0] != "2" {
						t.Fatalf("failed statement changed rows: %v", values)
					}
				})
			}
		}
	}
}
