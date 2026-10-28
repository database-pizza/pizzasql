package pgserver

import (
	"bufio"
	"bytes"
	"net"
	"testing"
)

func runExtendedMessage(t *testing.T, c *Connection, client net.Conn, msg *Message) *Message {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- c.handleExecute(msg)
	}()
	m, err := ReadMessage(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("handle message: %v", err)
	}
	return m
}

func TestNonConstantDefaultRejectedOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"), 0)})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want ErrorResponse+ReadyForQuery", len(msgs))
	}
	if code := errorCode(msgs[0]); code != ErrCodeFeatureNotSupported {
		t.Fatalf("error code = %q, want %q", code, ErrCodeFeatureNotSupported)
	}
	if message := errorField(t, msgs[0], ErrorFieldMessage); !bytes.Contains([]byte(message), []byte("constant")) {
		t.Fatalf("error message = %q, want an explanation about constant defaults", message)
	}
}

func TestNonConstantDefaultRejectedOverExtendedQuery(t *testing.T) {
	c, client := newDataConnection(t)

	// Parse does not parse the SQL (it is parsed at Execute), so install the
	// portal directly; this exercises the extended-protocol execution error path.
	c.portals["p1"] = &portal{query: "CREATE TABLE t (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"}

	execute := NewMessageBuilder()
	execute.WriteString("p1")
	execute.WriteInt32(0)
	errMsg := runExtendedMessage(t, c, client, &Message{Type: MsgExecute, Data: execute.Bytes()})
	if code := errorCode(errMsg); code != ErrCodeFeatureNotSupported {
		t.Fatalf("error code = %q, want %q", code, ErrCodeFeatureNotSupported)
	}
}
