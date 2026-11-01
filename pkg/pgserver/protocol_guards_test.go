package pgserver

import (
	"bufio"
	"testing"
)

func guardConnection(t *testing.T) (*Connection, *countingConn) {
	t.Helper()
	c, _ := newDataConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	if _, err := c.executor.Execute(parseStmt(t, "CREATE TABLE t (id INTEGER PRIMARY KEY)")); err != nil {
		t.Fatal(err)
	}
	return c, conn
}

func guardBind(parameterFormats, resultFormats []int16) []byte {
	b := NewMessageBuilder()
	b.WriteString("p")
	b.WriteString("q")
	b.WriteInt16(int16(len(parameterFormats)))
	for _, format := range parameterFormats {
		b.WriteInt16(format)
	}
	b.WriteInt16(0)
	b.WriteInt16(int16(len(resultFormats)))
	for _, format := range resultFormats {
		b.WriteInt16(format)
	}
	return b.Bytes()
}

func TestUnsupportedExtendedRequestsFailBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message *Message
		code    string
	}{
		{"binary_results", &Message{Type: MsgBind, Data: guardBind(nil, []int16{1})}, ErrCodeFeatureNotSupported},
		{"per_column_results", &Message{Type: MsgBind, Data: guardBind(nil, []int16{0, 0})}, ErrCodeFeatureNotSupported},
		{"invalid_result_format", &Message{Type: MsgBind, Data: guardBind(nil, []int16{2})}, ErrCodeProtocolViolation},
		{"invalid_parameter_format", &Message{Type: MsgBind, Data: guardBind([]int16{2}, nil)}, ErrCodeProtocolViolation},
		{"parameter_format_count", &Message{Type: MsgBind, Data: guardBind([]int16{0, 0}, nil)}, ErrCodeProtocolViolation},
		{"describe_statement", &Message{Type: MsgDescribe, Data: []byte{'S', 'q', 0}}, ErrCodeFeatureNotSupported},
		{"describe_portal", &Message{Type: MsgDescribe, Data: []byte{'P', 'p', 0}}, ErrCodeFeatureNotSupported},
		{"bounded_execute", &Message{Type: MsgExecute, Data: []byte{'p', 0, 0, 0, 0, 1}}, ErrCodeFeatureNotSupported},
		{"negative_execute", &Message{Type: MsgExecute, Data: []byte{'p', 0, 255, 255, 255, 255}}, ErrCodeProtocolViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, conn := guardConnection(t)
			query := "INSERT INTO t VALUES (1) RETURNING id"
			c.statements["q"] = &preparedStatement{query: query}
			c.portals["p"] = &portal{query: query}
			if err := c.handleMessage(tc.message); err != nil {
				t.Fatal(err)
			}
			// Requests pipelined after the error must be discarded until Sync.
			if err := c.handleMessage(&Message{Type: MsgExecute, Data: []byte{'p', 0, 0, 0, 0, 0}}); err != nil {
				t.Fatal(err)
			}
			if err := c.handleMessage(&Message{Type: MsgSync}); err != nil {
				t.Fatal(err)
			}
			messages := readAllMessages(t, conn.bytes())
			if len(messages) != 2 || errorCode(messages[0]) != tc.code || messages[1].Type != MsgReadyForQuery {
				t.Fatalf("unexpected recovery: %v", messages)
			}
			if result, err := c.executor.Execute(parseStmt(t, "SELECT COUNT(*) FROM t")); err != nil || result.Rows[0][0] != int64(0) {
				t.Fatalf("rejected request had side effects: %v %v", result, err)
			}
		})
	}
}

func TestDescribeFailurePreservesFailedTransactionState(t *testing.T) {
	c, conn := guardConnection(t)
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: []byte("BEGIN\x00")}); err != nil {
		t.Fatal(err)
	}
	c.portals["p"] = &portal{query: "INSERT INTO t VALUES (1) RETURNING id"}
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: []byte{'P', 'p', 0}}); err != nil {
		t.Fatal(err)
	}
	if err := c.handleMessage(&Message{Type: MsgSync}); err != nil {
		t.Fatal(err)
	}
	messages := readAllMessages(t, conn.bytes())
	last := messages[len(messages)-1]
	if last.Type != MsgReadyForQuery || len(last.Data) != 1 || last.Data[0] != TxStatusFailed {
		t.Fatalf("Describe did not fail transaction: %v", last)
	}
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")}); err != nil {
		t.Fatal(err)
	}
	if c.txStatus != TxStatusIdle {
		t.Fatal("rollback did not recover")
	}
}

func TestParseRejectsMultipleCommands(t *testing.T) {
	c, conn := guardConnection(t)
	parse := NewMessageBuilder()
	parse.WriteString("q")
	parse.WriteString("SELECT 1; INSERT INTO t VALUES (1)")
	parse.WriteInt16(0)
	if err := c.handleMessage(&Message{Type: MsgParse, Data: parse.Bytes()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.statements["q"]; ok {
		t.Fatal("multiple commands stored")
	}
	messages := readAllMessages(t, conn.bytes())
	if len(messages) != 1 || errorCode(messages[0]) != ErrCodeSyntaxError {
		t.Fatal(messages)
	}
}
