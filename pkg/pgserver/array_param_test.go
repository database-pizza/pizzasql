package pgserver

import (
	"bufio"
	"testing"
)

// A pg client that binds a text[] parameter for "id = ANY($1)" must receive a
// clear feature-not-supported error instead of a silently empty result.
func TestArrayParameterRejectedOverExtendedQuery(t *testing.T) {
	c, client := newDataConnection(t)
	reader := bufio.NewReader(client)

	parse := NewMessageBuilder()
	parse.WriteString("s1")
	parse.WriteString("SELECT * FROM t WHERE id = ANY($1)")
	parse.WriteInt16(1)
	parse.WriteInt32(1009) // text[]
	if err := c.handleParse(&Message{Type: MsgParse, Data: parse.Bytes()}); err != nil {
		t.Fatalf("handleParse: %v", err)
	}

	bind := NewMessageBuilder()
	bind.WriteString("p1")
	bind.WriteString("s1")
	bind.WriteInt16(0) // all-text parameter formats
	bind.WriteInt16(1) // one parameter
	bind.WriteInt32(5)
	bind.WriteBytes([]byte("{1,2}"))
	bind.WriteInt16(0) // all-text result formats
	if err := c.handleBind(&Message{Type: MsgBind, Data: bind.Bytes()}); err != nil {
		t.Fatalf("handleBind: %v", err)
	}
	execute := NewMessageBuilder()
	execute.WriteString("p1")
	execute.WriteInt32(0)
	done := make(chan error, 1)
	// handleBind buffered BindComplete without flushing; handleExecute's error
	// path flushes both messages, so read them in order.
	go func() { done <- c.handleExecute(&Message{Type: MsgExecute, Data: execute.Bytes()}) }()
	var errMsg *Message
	for errMsg == nil {
		msg, err := ReadMessage(reader)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if msg.Type == MsgErrorResponse {
			errMsg = msg
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("handleExecute: %v", err)
	}
	if code := errorCode(errMsg); code != ErrCodeFeatureNotSupported {
		t.Fatalf("error code = %q, want %q", code, ErrCodeFeatureNotSupported)
	}
}
