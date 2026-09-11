package pgserver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// newDataConnection builds a connection with a real executor backed by testkv so
// simple-query behavior can be asserted end to end.
func newDataConnection(t *testing.T) (*Connection, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	kv := testkv.New(t)
	pool := kv.Pool(4)
	t.Cleanup(func() { pool.Close() })
	schema := storage.NewSchemaManager(pool, "pg_features")
	table := storage.NewTableManager(pool, schema, "pg_features")
	exec := executor.New(schema, table)
	exec.SyncCatalog()

	c := &Connection{
		conn:       server,
		reader:     bufio.NewReader(server),
		writer:     bufio.NewWriter(server),
		params:     map[string]string{"user": "tester"},
		statements: make(map[string]*preparedStatement),
		portals:    make(map[string]*portal),
		txStatus:   TxStatusIdle,
		quiet:      true,
		executor:   exec,
		schema:     schema,
	}
	return c, client
}

// dataRowValues decodes a DataRow message into its text values.
func dataRowValues(t *testing.T, msg *Message) []string {
	t.Helper()
	if msg.Type != MsgDataRow {
		t.Fatalf("message type = %c, want DataRow", msg.Type)
	}
	data := msg.Data
	if len(data) < 2 {
		t.Fatal("short DataRow")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	pos := 2
	values := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if pos+4 > len(data) {
			t.Fatal("short DataRow field length")
		}
		l := int32(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if l == -1 {
			values = append(values, "<null>")
			continue
		}
		values = append(values, string(data[pos:pos+int(l)]))
		pos += int(l)
	}
	return values
}

// commandTag extracts the NUL-terminated tag from a CommandComplete message.
func commandTag(t *testing.T, msg *Message) string {
	t.Helper()
	if msg.Type != MsgCommandComplete {
		t.Fatalf("message type = %c, want CommandComplete", msg.Type)
	}
	return string(bytes.TrimRight(msg.Data, "\x00"))
}

func TestSimpleQueryInsertReturning(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (name) VALUES ('alice') RETURNING id, name"), 0)})
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want RowDescription+DataRow+CommandComplete+ReadyForQuery: %v", len(msgs), msgs)
	}
	if msgs[0].Type != MsgRowDescription {
		t.Fatalf("first message = %c, want RowDescription", msgs[0].Type)
	}
	values := dataRowValues(t, msgs[1])
	if len(values) != 2 || values[0] != "1" || values[1] != "alice" {
		t.Fatalf("returning row = %v", values)
	}
	if tag := commandTag(t, msgs[2]); tag != "INSERT 0 1" {
		t.Fatalf("command tag = %q, want INSERT 0 1", tag)
	}
}

func TestSimpleQueryByteaTextWire(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE b (id INTEGER PRIMARY KEY, data BLOB)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO b (id, data) VALUES (1, X'00FF10')"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT data FROM b WHERE id = 1"), 0)})
	values := dataRowValues(t, msgs[1])
	if len(values) != 1 || values[0] != `\x00ff10` {
		t.Fatalf("bytea wire value = %v, want \\x00ff10", values)
	}
}

func TestBlobValueOverridesTextAffinityWireType(t *testing.T) {
	result := executor.NewResult("SELECT")
	result.AddColumnWithType("settings", "VARCHAR")
	result.AddRow([]byte(`{"collect":1}`))

	types := wireColumnTypes(result)
	if len(types) != 1 || types[0] != "BLOB" {
		t.Fatalf("wire types = %v, want [BLOB]", types)
	}
	if result.ColumnTypes[0] != "VARCHAR" {
		t.Fatalf("wire type inference mutated result metadata: %v", result.ColumnTypes)
	}
}

func TestCommandTagSelectAndUpdate(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t VALUES (1, 'a')"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT * FROM t"), 0)})
	if tag := commandTag(t, msgs[len(msgs)-2]); tag != "SELECT 1" {
		t.Fatalf("select tag = %q", tag)
	}

	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("UPDATE t SET v='b' RETURNING id"), 0)})
	if msgs[0].Type != MsgRowDescription {
		t.Fatalf("update returning first message = %c", msgs[0].Type)
	}
	if tag := commandTag(t, msgs[len(msgs)-2]); tag != "UPDATE 1" {
		t.Fatalf("update tag = %q", tag)
	}
}

func TestGetCommandTagSelectUsesRowCount(t *testing.T) {
	c := &Connection{txStatus: TxStatusIdle}
	res := executor.NewResult("SELECT")
	res.AddRow(1)
	res.AddRow(2)
	if tag := c.getCommandTag(parseStmt(t, "SELECT 1"), res); tag != "SELECT 2" {
		t.Fatalf("tag = %q, want SELECT 2", tag)
	}
}
