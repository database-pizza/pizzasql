package pgserver

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

func TestBindQuery(t *testing.T) {
	query, err := bindQuery(
		"SELECT $1, $2, $3, $4, '$5'",
		[]boundParameter{
			{value: []byte("O'Reilly")},
			{value: []byte("42")},
			{null: true},
			{value: []byte("true")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT pizzasql_param('O''Reilly'), pizzasql_param(42), pizzasql_param(NULL), pizzasql_param(TRUE), '$5'"
	if query != want {
		t.Fatalf("bound query = %q, want %q", query, want)
	}
}

func TestBindQueryRequiresEveryParameter(t *testing.T) {
	if _, err := bindQuery("SELECT $2", []boundParameter{{value: []byte("one")}}); err == nil {
		t.Fatal("expected missing parameter error")
	}
}

func parseStmt(t *testing.T, sql string) parser.Statement {
	t.Helper()
	l := lexer.New(sql)
	p := parser.New(l)
	stmt, err := p.Parse()
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return stmt
}

func TestGetCommandTagSavepointKeepsTransaction(t *testing.T) {
	c := &Connection{txStatus: TxStatusIdle}
	tag := c.getCommandTag(parseStmt(t, "SAVEPOINT sp1"), executor.NewResult("SAVEPOINT"))
	if tag != "SAVEPOINT" {
		t.Fatalf("tag = %q, want %q", tag, "SAVEPOINT")
	}
	if c.txStatus != TxStatusInBlock {
		t.Fatalf("txStatus = %c, want %c", c.txStatus, TxStatusInBlock)
	}
}

func TestGetCommandTagReleaseKeepsTransaction(t *testing.T) {
	c := &Connection{txStatus: TxStatusInBlock}
	tag := c.getCommandTag(parseStmt(t, "RELEASE SAVEPOINT sp1"), executor.NewResult("RELEASE"))
	if tag != "RELEASE" {
		t.Fatalf("tag = %q, want %q", tag, "RELEASE")
	}
	if c.txStatus != TxStatusInBlock {
		t.Fatalf("txStatus = %c, want %c", c.txStatus, TxStatusInBlock)
	}
}

func TestGetCommandTagRollbackToSavepointKeepsTransaction(t *testing.T) {
	c := &Connection{txStatus: TxStatusFailed}
	tag := c.getCommandTag(parseStmt(t, "ROLLBACK TO SAVEPOINT sp1"), executor.NewResult("ROLLBACK"))
	if tag != "ROLLBACK" {
		t.Fatalf("tag = %q, want %q", tag, "ROLLBACK")
	}
	if c.txStatus != TxStatusInBlock {
		t.Fatalf("txStatus = %c, want %c", c.txStatus, TxStatusInBlock)
	}
}

func TestGetCommandTagFullRollbackEndsTransaction(t *testing.T) {
	c := &Connection{txStatus: TxStatusFailed}
	tag := c.getCommandTag(parseStmt(t, "ROLLBACK"), executor.NewResult("ROLLBACK"))
	if tag != "ROLLBACK" {
		t.Fatalf("tag = %q, want %q", tag, "ROLLBACK")
	}
	if c.txStatus != TxStatusIdle {
		t.Fatalf("txStatus = %c, want %c", c.txStatus, TxStatusIdle)
	}
}

func newTestConnection(t *testing.T) (*Connection, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	c := &Connection{
		conn:       server,
		reader:     bufio.NewReader(server),
		writer:     bufio.NewWriter(server),
		params:     map[string]string{"user": "tester"},
		statements: make(map[string]*preparedStatement),
		portals:    make(map[string]*portal),
		txStatus:   TxStatusIdle,
		quiet:      true,
	}
	return c, client
}

func runQuery(t *testing.T, c *Connection, client net.Conn, msg *Message) []*Message {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.handleQuery(msg) }()

	reader := bufio.NewReader(client)
	var msgs []*Message
	for {
		m, err := ReadMessage(reader)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		msgs = append(msgs, m)
		if m.Type == MsgReadyForQuery {
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	return msgs
}

func errorCode(msg *Message) string {
	if msg == nil || msg.Type != MsgErrorResponse {
		return ""
	}
	data := msg.Data
	for i := 0; i < len(data); {
		if data[i] == 0 {
			break
		}
		field := data[i]
		i++
		end := bytes.IndexByte(data[i:], 0)
		if end < 0 {
			break
		}
		value := string(data[i : i+end])
		if field == ErrorFieldCode {
			return value
		}
		i += end + 1
	}
	return ""
}

func TestHandleQueryEmptyPayloadReturnsProtocolError(t *testing.T) {
	c, client := newTestConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte{}})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if code := errorCode(msgs[0]); code != ErrCodeProtocolViolation {
		t.Fatalf("error code = %q, want %q", code, ErrCodeProtocolViolation)
	}
}

func TestHandleQueryMissingNullTerminatorReturnsProtocolError(t *testing.T) {
	c, client := newTestConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT 1")})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if code := errorCode(msgs[0]); code != ErrCodeProtocolViolation {
		t.Fatalf("error code = %q, want %q", code, ErrCodeProtocolViolation)
	}
}

func TestHandleQueryEmptyQueryReturnsEmptyQueryResponse(t *testing.T) {
	for _, data := range [][]byte{{0}, []byte(";\x00"), []byte("   \x00")} {
		c, client := newTestConnection(t)
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: data})
		if len(msgs) != 2 {
			t.Fatalf("payload %q: got %d messages, want 2", data, len(msgs))
		}
		if msgs[0].Type != MsgEmptyQueryResponse {
			t.Fatalf("payload %q: first message type = %c, want %c", data, msgs[0].Type, MsgEmptyQueryResponse)
		}
	}
}

func TestHandleQueryFailedTransactionInterceptsSpecialQueries(t *testing.T) {
	queries := []string{
		"SELECT version()",
		"SELECT current_user",
		"SHOW server_version",
		"SELECT * FROM information_schema.tables",
	}
	for _, q := range queries {
		c, client := newTestConnection(t)
		c.txStatus = TxStatusFailed
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte(q + "\x00")})
		if len(msgs) != 2 {
			t.Fatalf("query %q: got %d messages, want 2", q, len(msgs))
		}
		if code := errorCode(msgs[0]); code != ErrCodeTransactionAborted {
			t.Fatalf("query %q: error code = %q, want %q", q, code, ErrCodeTransactionAborted)
		}
	}
}

func TestIsRollbackStatement(t *testing.T) {
	for _, sql := range []string{"ROLLBACK", "ROLLBACK TO SAVEPOINT sp1", "rollback;", "  rollback  ;"} {
		if !isRollbackStatement(sql) {
			t.Errorf("isRollbackStatement(%q) = false, want true", sql)
		}
	}
	for _, sql := range []string{"SELECT version()", "SHOW server_version", "SAVEPOINT sp1"} {
		if isRollbackStatement(sql) {
			t.Errorf("isRollbackStatement(%q) = true, want false", sql)
		}
	}
	for _, sql := range []string{"ROLLBACKBOGUS", "ROLLBACK; SELECT 1"} {
		if isRollbackStatement(sql) {
			t.Errorf("isRollbackStatement(%q) = true, want false", sql)
		}
	}
}

// countingConn is an in-memory net.Conn that records how many times the
// underlying stream is written. bufio.Writer flushes produce exactly one write
// call each, so this counts flushes without blocking like net.Pipe does.
type countingConn struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes int
}

func (c *countingConn) Read(p []byte) (int, error) { return 0, io.EOF }

func (c *countingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	return c.buf.Write(p)
}

func (c *countingConn) Close() error                       { return nil }
func (c *countingConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *countingConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *countingConn) SetDeadline(t time.Time) error      { return nil }
func (c *countingConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *countingConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *countingConn) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func newCountingTestConnection(t *testing.T) (*Connection, *countingConn) {
	t.Helper()
	cc := &countingConn{}
	c := &Connection{
		conn:       cc,
		reader:     bufio.NewReader(cc),
		writer:     bufio.NewWriter(cc),
		params:     map[string]string{"user": "tester"},
		statements: make(map[string]*preparedStatement),
		portals:    make(map[string]*portal),
		txStatus:   TxStatusIdle,
		quiet:      true,
	}
	return c, cc
}

func readAllMessages(t *testing.T, data []byte) []*Message {
	t.Helper()
	r := bytes.NewReader(data)
	var msgs []*Message
	for r.Len() > 0 {
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestWriteMessageDoesNotFlush(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.writeMessage(MsgCommandComplete, []byte("SELECT 0")); err != nil {
		t.Fatal(err)
	}
	if cc.writes != 0 {
		t.Fatalf("writeMessage flushed %d times, want 0", cc.writes)
	}
}

func TestReadyForQueryFlushesBufferedMessages(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.writeMessage(MsgCommandComplete, []byte("SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if err := c.sendReadyForQuery(); err != nil {
		t.Fatal(err)
	}
	if cc.writes == 0 {
		t.Fatal("sendReadyForQuery did not flush buffered messages")
	}
	msgs := readAllMessages(t, cc.bytes())
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Type != MsgCommandComplete || msgs[1].Type != MsgReadyForQuery {
		t.Fatalf("unexpected message sequence: %c, %c", msgs[0].Type, msgs[1].Type)
	}
}

func TestErrorResponseFlushes(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.sendError("ERROR", ErrCodeSyntaxError, "boom"); err != nil {
		t.Fatal(err)
	}
	if cc.writes == 0 {
		t.Fatal("sendError did not flush")
	}
	msgs := readAllMessages(t, cc.bytes())
	if len(msgs) != 1 || msgs[0].Type != MsgErrorResponse {
		t.Fatalf("expected a single error response, got %d messages", len(msgs))
	}
}

func TestFlushMessageFlushes(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.writeMessage(MsgCommandComplete, []byte("SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if err := c.handleMessage(&Message{Type: MsgFlush}); err != nil {
		t.Fatal(err)
	}
	if cc.writes == 0 {
		t.Fatal("Flush message did not flush buffered data")
	}
}

func TestMultiRowResultBuffersRows(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	result := executor.NewResult("SELECT")
	result.AddColumnWithType("n", "INTEGER")
	const rows = 500
	for i := 0; i < rows; i++ {
		result.AddRow(int64(i))
	}
	if err := c.sendResult(result, &parser.SelectStmt{}, result.ColumnTypes, nil); err != nil {
		t.Fatal(err)
	}
	// sendResult emits RowDescription + rows DataRows + CommandComplete. With
	// per-message flushing that would be rows+2 underlying writes; buffered it
	// is bounded by the bufio.Writer capacity (a few flushes at most).
	numMessages := rows + 2
	if cc.writes >= numMessages {
		t.Fatalf("sendResult flushed %d times, want fewer than %d messages", cc.writes, numMessages)
	}
	if cc.writes >= rows {
		t.Fatalf("sendResult flushed %d times for %d rows, expected buffering", cc.writes, rows)
	}
}
