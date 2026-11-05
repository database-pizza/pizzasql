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

func TestDescribeDoesNotExecuteOrFailTransaction(t *testing.T) {
	c, conn := guardConnection(t)
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: []byte("BEGIN\x00")}); err != nil {
		t.Fatal(err)
	}
	c.statements["q"] = &preparedStatement{query: "INSERT INTO t VALUES (1) RETURNING id", paramOIDs: []int32{20}}
	c.portals["p"] = &portal{query: "INSERT INTO t VALUES (1) RETURNING id"}
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: []byte{'S', 'q', 0}}); err != nil {
		t.Fatal(err)
	}
	if err := c.handleMessage(&Message{Type: MsgDescribe, Data: []byte{'P', 'p', 0}}); err != nil {
		t.Fatal(err)
	}
	if err := c.handleMessage(&Message{Type: MsgSync}); err != nil {
		t.Fatal(err)
	}
	messages := readAllMessages(t, conn.bytes())
	seenParam, seenNoData := false, 0
	for _, message := range messages {
		switch message.Type {
		case MsgParameterDescription:
			seenParam = true
		case MsgNoData:
			seenNoData++
		case MsgErrorResponse:
			t.Fatalf("Describe failed: %s", errorCode(message))
		}
	}
	if !seenParam || seenNoData != 2 {
		t.Fatalf("Describe messages = %v", messages)
	}
	if c.txStatus != TxStatusInBlock || c.extendedFailed {
		t.Fatalf("Describe changed transaction state: %c failed=%v", c.txStatus, c.extendedFailed)
	}
	if result, err := c.executor.Execute(parseStmt(t, "SELECT COUNT(*) FROM t")); err != nil || result.Rows[0][0] != int64(0) {
		t.Fatalf("Describe executed a side effect: %v %v", result, err)
	}
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: []byte("ROLLBACK\x00")}); err != nil {
		t.Fatal(err)
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

func TestTypicalExtendedSequenceWithDescribeSucceeds(t *testing.T) {
	c, conn := guardConnection(t)
	if _, err := c.executor.Execute(parseStmt(t, "INSERT INTO t VALUES (7)")); err != nil {
		t.Fatal(err)
	}
	parse := NewMessageBuilder()
	parse.WriteString("")
	parse.WriteString("SELECT id FROM t WHERE id = $1")
	parse.WriteInt16(1)
	parse.WriteInt32(20)
	bind := NewMessageBuilder()
	bind.WriteString("")
	bind.WriteString("")
	bind.WriteInt16(0)
	bind.WriteInt16(1)
	bind.WriteInt32(1)
	bind.WriteBytes([]byte("7"))
	bind.WriteInt16(0)
	execute := NewMessageBuilder()
	execute.WriteString("")
	execute.WriteInt32(0)
	for _, msg := range []*Message{
		{Type: MsgParse, Data: parse.Bytes()},
		{Type: MsgBind, Data: bind.Bytes()},
		{Type: MsgDescribe, Data: []byte{'P', 0}},
		{Type: MsgExecute, Data: execute.Bytes()},
		{Type: MsgSync},
	} {
		if err := c.handleMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	messages := readAllMessages(t, conn.bytes())
	var types []byte
	var row []string
	for _, message := range messages {
		types = append(types, message.Type)
		if message.Type == MsgErrorResponse {
			t.Fatalf("extended SELECT failed: %s", errorCode(message))
		}
		if message.Type == MsgDataRow {
			row = dataRowValues(t, message)
		}
	}
	if string(types) != "12tnTDCZ" {
		t.Fatalf("message sequence = %q", types)
	}
	if len(row) != 1 || row[0] != "7" {
		t.Fatalf("row = %v", row)
	}
}
