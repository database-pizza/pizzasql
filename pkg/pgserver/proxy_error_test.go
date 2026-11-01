package pgserver

import (
	"bufio"
	"net"
	"testing"
)

func runMessageCycle(t *testing.T, c *Connection, client net.Conn, messages ...*Message) []*Message {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for _, msg := range messages {
			if err := c.handleMessage(msg); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	r := bufio.NewReader(client)
	var response []*Message
	for {
		msg, err := ReadMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		response = append(response, msg)
		if msg.Type == MsgReadyForQuery {
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSyntaxErrorAbortsSimpleAndExtendedTransaction(t *testing.T) {
	for _, extended := range []bool{false, true} {
		c, client := newDataConnection(t)
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("BEGIN\x00")})
		var response []*Message
		if extended {
			response = runExtendedExecution(t, c, client, "SELEC invalid")
		} else {
			response = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELEC invalid\x00")})
		}
		if errorCode(response[0]) != "42601" || c.txStatus != TxStatusFailed {
			t.Fatalf("extended=%v did not abort on syntax error", extended)
		}
		response = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT 1\x00")})
		if errorCode(response[0]) != "25P02" {
			t.Fatal("failed transaction accepted SELECT")
		}
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")})
	}
}

func TestProxyErrorRelayTransactionAndSyncRecovery(t *testing.T) {
	for _, extended := range []bool{false, true} {
		c, client := newDataConnection(t)
		for _, sql := range []string{"CREATE TABLE t (id INTEGER PRIMARY KEY)", "BEGIN", "INSERT INTO t VALUES (1)", "SAVEPOINT s"} {
			runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte(sql + "\x00")})
		}
		kind := byte(MsgQuery)
		if extended {
			kind = MsgParse
		}
		messages := []*Message{{Type: MsgProxyError, Data: []byte{kind}}}
		if extended {
			c.portals[""] = &portal{query: "INSERT INTO t VALUES (2)"}
			messages = append(messages, &Message{Type: MsgExecute, Data: []byte{0, 0, 0, 0, 0}}, &Message{Type: MsgSync})
		}
		response := runMessageCycle(t, c, client, messages...)
		if errorCode(response[0]) != "42501" {
			t.Fatalf("extended=%v missing permission error", extended)
		}
		if last := response[len(response)-1]; last.Type != MsgReadyForQuery || string(last.Data) != "E" {
			t.Fatalf("extended=%v false ready state: %v", extended, last)
		}
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK TO SAVEPOINT s\x00")})
		response = runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT COUNT(*) FROM t\x00")})
		if row := dataRowValues(t, response[1]); len(row) != 1 || row[0] != "1" {
			t.Fatalf("relay lost earlier writes or executed discarded messages: %v", row)
		}
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")})
	}
}
