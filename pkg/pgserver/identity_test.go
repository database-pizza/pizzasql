package pgserver

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

func startupMessage(params map[string]string) []byte {
	size := 4 + 4
	for k, v := range params {
		size += len(k) + 1 + len(v) + 1
	}
	size++ // terminator
	buf := make([]byte, 0, size)
	buf = binary.BigEndian.AppendUint32(buf, uint32(size))
	buf = binary.BigEndian.AppendUint32(buf, 196608) // protocol 3.0
	for k, v := range params {
		buf = append(buf, k...)
		buf = append(buf, 0)
		buf = append(buf, v...)
		buf = append(buf, 0)
	}
	return append(buf, 0)
}

func TestStartupReportsBuildIdentitySeparately(t *testing.T) {
	kv := testkv.New(t)
	pool := kv.Pool(2)
	t.Cleanup(func() { pool.Close() })
	dbManager := storage.NewDatabaseManager(pool, nil)

	server, client := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := NewConnection(server, dbManager, true)
	done := make(chan error, 1)
	go func() { done <- conn.Handle() }()

	go client.Write(startupMessage(map[string]string{"user": "tester", "database": "identity"}))

	reader := bufio.NewReader(client)
	statuses := map[string]string{}
	for {
		msg, err := ReadMessage(reader)
		if err != nil {
			t.Fatalf("read startup response: %v", err)
		}
		if msg.Type == MsgParameterStatus {
			data := msg.Data
			key, pos, err := readCString(data, 0)
			if err != nil {
				t.Fatalf("ParameterStatus key: %v", err)
			}
			value, _, err := readCString(data, pos)
			if err != nil {
				t.Fatalf("ParameterStatus value: %v", err)
			}
			statuses[key] = value
		}
		if msg.Type == MsgReadyForQuery {
			break
		}
	}

	if got := statuses["server_version"]; got != version.PostgreSQLCompatVersion+" (PizzaSQL)" {
		t.Fatalf("server_version = %q, want PostgreSQL compatibility string", got)
	}
	if got := statuses["pizzasql.version"]; got != version.String() {
		t.Fatalf("pizzasql.version = %q, want %q", got, version.String())
	}
	if got := statuses["pizzasql.commit"]; got != version.Commit {
		t.Fatalf("pizzasql.commit = %q, want %q", got, version.Commit)
	}

	client.Write([]byte{'X', 0, 0, 0, 4}) // Terminate
	if err := <-done; err != nil && err != io.EOF {
		t.Fatalf("Handle: %v", err)
	}
}

func TestVersionFunctionKeepsPostgresCompatibilityString(t *testing.T) {
	c, client := newDataConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT version()"), 0)})
	values := dataRowValues(t, msgs[1])
	if len(values) != 1 || values[0] != "PostgreSQL "+version.PostgreSQLCompatVersion+" (PizzaSQL)" {
		t.Fatalf("version() = %v", values)
	}
}

func TestPizzasqlVersionFunctionReturnsBuild(t *testing.T) {
	c, client := newDataConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT pizzasql_version()"), 0)})
	values := dataRowValues(t, msgs[1])
	if len(values) != 1 || values[0] != version.String() {
		t.Fatalf("pizzasql_version() = %v, want %q", values, version.String())
	}
}
