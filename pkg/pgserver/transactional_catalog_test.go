package pgserver

import (
	"net"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// catalogRows decodes every DataRow of a completed simple-query exchange.
func catalogRows(t *testing.T, msgs []*Message) [][]string {
	t.Helper()
	var rows [][]string
	for _, msg := range msgs {
		if msg.Type == MsgDataRow {
			rows = append(rows, dataRowValues(t, msg))
		}
	}
	return rows
}

func newSharedDataConnections(t *testing.T) (*Connection, net.Conn, *Connection, net.Conn) {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(8)
	t.Cleanup(func() { pool.Close() })
	schema := storage.NewSchemaManager(pool, "pg_tx_catalog")
	table := storage.NewTableManager(pool, schema, "pg_tx_catalog")
	writer, writerClient := newDataConnectionFor(t, schema, table)
	observer, observerClient := newDataConnectionFor(t, schema, table)
	return writer, writerClient, observer, observerClient
}

// The system-catalog emulation must read through the connection's session so it
// sees DDL staged by an open transaction, and must keep showing the committed
// catalog to other connections until that transaction commits.
func TestCatalogEmulationSeesStagedDDLInTransaction(t *testing.T) {
	writer, writerClient, observer, observerClient := newSharedDataConnections(t)

	runQuery(t, writer, writerClient, simpleQueryMessage("CREATE TABLE base (id INTEGER PRIMARY KEY)"))
	runQuery(t, writer, writerClient, simpleQueryMessage("BEGIN"))
	runQuery(t, writer, writerClient, simpleQueryMessage("CREATE TABLE staged (id INTEGER PRIMARY KEY)"))
	runQuery(t, writer, writerClient, simpleQueryMessage("ALTER TABLE base ADD COLUMN note TEXT"))
	runQuery(t, writer, writerClient, simpleQueryMessage("CREATE INDEX idx_base_note ON base (note)"))

	hasTable := func(msgs []*Message, want string) bool {
		for _, row := range catalogRows(t, msgs) {
			if len(row) > 0 && row[0] == want {
				return true
			}
		}
		return false
	}

	// The transaction's own session sees its staged catalog.
	tables := runQuery(t, writer, writerClient, simpleQueryMessage("SELECT table_name FROM information_schema.tables"))
	if !hasTable(tables, "staged") {
		t.Fatal("transactional catalog emulation did not report a staged CREATE TABLE")
	}
	columns := runQuery(t, writer, writerClient, simpleQueryMessage("SELECT column_name FROM information_schema.columns"))
	if !hasTable(columns, "note") {
		t.Fatal("transactional catalog emulation did not report a staged ADD COLUMN")
	}
	indexes := runQuery(t, writer, writerClient, simpleQueryMessage("SELECT indexname FROM pg_indexes"))
	if !hasTable(indexes, "idx_base_note") {
		t.Fatal("transactional catalog emulation did not report a staged CREATE INDEX")
	}

	// Another connection keeps reading the committed catalog.
	observerTables := runQuery(t, observer, observerClient, simpleQueryMessage("SELECT table_name FROM information_schema.tables"))
	if hasTable(observerTables, "staged") {
		t.Fatal("observer saw a table created in another connection's transaction")
	}
	observerColumns := runQuery(t, observer, observerClient, simpleQueryMessage("SELECT column_name FROM information_schema.columns"))
	if hasTable(observerColumns, "note") {
		t.Fatal("observer saw a column added in another connection's transaction")
	}

	runQuery(t, writer, writerClient, simpleQueryMessage("COMMIT"))

	// After COMMIT the committed catalog reflects the migration for everyone.
	observerTables = runQuery(t, observer, observerClient, simpleQueryMessage("SELECT table_name FROM information_schema.tables"))
	if !hasTable(observerTables, "staged") {
		t.Fatal("observer did not see the committed CREATE TABLE")
	}
	observerColumns = runQuery(t, observer, observerClient, simpleQueryMessage("SELECT column_name FROM information_schema.columns"))
	if !hasTable(observerColumns, "note") {
		t.Fatal("observer did not see the committed ADD COLUMN")
	}
	observerIndexes := runQuery(t, observer, observerClient, simpleQueryMessage("SELECT indexname FROM pg_indexes"))
	if !hasTable(observerIndexes, "idx_base_note") {
		t.Fatal("observer did not see the committed CREATE INDEX")
	}

	// A ROLLBACK leaves the emulation on the committed catalog again.
	runQuery(t, writer, writerClient, simpleQueryMessage("BEGIN"))
	runQuery(t, writer, writerClient, simpleQueryMessage("CREATE TABLE rolled_back (id INTEGER PRIMARY KEY)"))
	if tables := runQuery(t, writer, writerClient, simpleQueryMessage("SELECT table_name FROM information_schema.tables")); !hasTable(tables, "rolled_back") {
		t.Fatal("transactional catalog emulation did not report a staged table before ROLLBACK")
	}
	runQuery(t, writer, writerClient, simpleQueryMessage("ROLLBACK"))
	if tables := runQuery(t, writer, writerClient, simpleQueryMessage("SELECT table_name FROM information_schema.tables")); hasTable(tables, "rolled_back") {
		t.Fatal("catalog emulation kept a rolled-back table")
	}
}
