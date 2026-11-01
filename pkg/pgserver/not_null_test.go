package pgserver

import (
	"bufio"
	"net"
	"testing"
)

func runExtendedExecution(t *testing.T, c *Connection, client net.Conn, sql string) []*Message {
	t.Helper()
	c.portals[""] = &portal{query: sql}
	done := make(chan error, 1)
	go func() {
		if err := c.handleExecute(&Message{Type: MsgExecute, Data: []byte{0, 0, 0, 0, 0}}); err != nil {
			done <- err
			return
		}
		done <- c.handleMessage(&Message{Type: MsgSync})
	}()
	reader := bufio.NewReader(client)
	var messages []*Message
	for {
		m, err := ReadMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
		if m.Type == MsgReadyForQuery {
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestNotNullSQLStateAndTransactionState(t *testing.T) {
	for _, extended := range []bool{false, true} {
		c, client := newDataConnection(t)
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL DEFAULT 'x')\x00")})
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("BEGIN\x00")})
		var messages []*Message
		if extended {
			messages = runExtendedExecution(t, c, client, "INSERT INTO t VALUES (1,NULL)")
		} else {
			messages = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("INSERT INTO t VALUES (1,NULL)\x00")})
		}
		if code := errorCode(messages[0]); code != "23502" {
			t.Fatalf("extended=%v SQLSTATE=%q", extended, code)
		}
		if c.txStatus != TxStatusFailed {
			t.Fatalf("extended=%v transaction not failed: %c", extended, c.txStatus)
		}
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")})
	}
}
