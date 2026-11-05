package pgserver

import (
	"strconv"
	"strings"
	"testing"
)

func simpleQuery(sql string) *Message {
	return &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}
}

// Catalog columns carry PostgreSQL's types over the wire (oid 26, name 19), so
// a driver decodes them without falling back to TEXT.
func TestPgCatalogWireTypes(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE wire (id INTEGER PRIMARY KEY, payload JSONB)",
		"CREATE INDEX idx_wire_payload ON wire (payload)",
	} {
		if msgs := runQuery(t, c, client, simpleQuery(sql)); errorCode(msgs[0]) != "" {
			t.Fatalf("%s: %s", sql, errorMessage(msgs[0]))
		}
	}

	msgs := runQuery(t, c, client, simpleQuery(`
		SELECT c.oid, c.relname, c.relkind
		FROM pg_catalog.pg_class c
		WHERE c.relname = 'wire'`))
	if msg := findError(msgs); msg != nil {
		t.Fatalf("catalog query failed: %s", errorMessage(msg))
	}
	rd := findRowDescription(t, msgs)
	if got := rowDescriptionOIDs(t, rd.Data); len(got) != 3 || got[0] != 26 || got[1] != 19 || got[2] != 25 {
		t.Fatalf("catalog OIDs = %v, want [26 19 25]", got)
	}
	rows := catalogRows(t, msgs)
	if len(rows) != 1 || rows[0][1] != "wire" || rows[0][2] != "r" {
		t.Fatalf("pg_class row = %v", rows)
	}
	if _, err := strconv.ParseUint(rows[0][0], 10, 32); err != nil {
		t.Fatalf("oid %q is not numeric: %v", rows[0][0], err)
	}
}

// A join across pg_class/pg_namespace/pg_attribute works over the simple-query
// protocol, which is how migration tools list columns.
func TestPgCatalogJoinOverWire(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE wire_join (id INTEGER PRIMARY KEY, label TEXT NOT NULL)",
	} {
		if msgs := runQuery(t, c, client, simpleQuery(sql)); errorCode(msgs[0]) != "" {
			t.Fatalf("%s: %s", sql, errorMessage(msgs[0]))
		}
	}
	msgs := runQuery(t, c, client, simpleQuery(`
		SELECT n.nspname, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE c.relname = 'wire_join' AND a.attnum > 0
		ORDER BY a.attnum`))
	if msg := findError(msgs); msg != nil {
		t.Fatalf("catalog join failed: %s", errorMessage(msg))
	}
	rows := catalogRows(t, msgs)
	if len(rows) != 2 || rows[0][1] != "id" || rows[0][2] != "integer" || rows[1][1] != "label" {
		t.Fatalf("catalog join rows = %v", rows)
	}
}

// ORM connections issue SET during startup; the engine acknowledges it and
// SHOW reports the retained search path.
func TestSetAndShowSearchPathOverWire(t *testing.T) {
	c, client := newDataConnection(t)
	if msgs := runQuery(t, c, client, simpleQuery("SET search_path TO public")); errorCode(msgs[0]) != "" {
		t.Fatalf("SET search_path failed: %s", errorMessage(msgs[0]))
	}
	if msgs := runQuery(t, c, client, simpleQuery("SET TIME ZONE 'UTC'")); errorCode(msgs[0]) != "" {
		t.Fatalf("SET TIME ZONE failed: %s", errorMessage(msgs[0]))
	}
	msgs := runQuery(t, c, client, simpleQuery("SHOW search_path"))
	if msg := findError(msgs); msg != nil {
		t.Fatalf("SHOW search_path failed: %s", errorMessage(msg))
	}
	rows := catalogRows(t, msgs)
	if len(rows) != 1 || rows[0][0] != "public" {
		t.Fatalf("SHOW search_path = %v", rows)
	}
	msgs = runQuery(t, c, client, simpleQuery("SHOW client_encoding"))
	if rows := catalogRows(t, msgs); len(rows) != 1 || !strings.EqualFold(rows[0][0], "UTF8") {
		t.Fatalf("SHOW client_encoding = %v", rows)
	}
}

// An unknown regclass/regtype reaches the client as SQLSTATE 42P01 over the
// simple-query protocol, both in a projection and inside a WHERE clause, and
// to_regclass returns NULL instead of failing.
func TestPgCatalogUndefinedRelationSQLState(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE wire_state (id INTEGER PRIMARY KEY)",
	} {
		if msgs := runQuery(t, c, client, simpleQuery(sql)); errorCode(msgs[0]) != "" {
			t.Fatalf("%s: %s", sql, errorMessage(msgs[0]))
		}
	}

	for _, sql := range []string{
		`SELECT 'missing_relation'::regclass`,
		`SELECT 'missing_type'::regtype`,
		`SELECT count(*) FROM pg_catalog.pg_class WHERE oid = 'missing_relation'::regclass`,
		`SELECT count(*) FROM public.pg_class`,
	} {
		msgs := runQuery(t, c, client, simpleQuery(sql))
		msg := findError(msgs)
		if msg == nil {
			t.Fatalf("%s: expected an error, got %v", sql, msgs)
		}
		if code := errorCode(msg); code != ErrCodeUndefinedTable {
			t.Fatalf("%s: SQLSTATE = %q, want %q", sql, code, ErrCodeUndefinedTable)
		}
	}

	msgs := runQuery(t, c, client, simpleQuery(`SELECT to_regclass('missing_relation'), to_regclass('wire_state')`))
	if msg := findError(msgs); msg != nil {
		t.Fatalf("to_regclass failed: %s", errorMessage(msg))
	}
	rd := findRowDescription(t, msgs)
	if got := rowDescriptionOIDs(t, rd.Data); len(got) != 2 || got[0] != 2205 || got[1] != 2205 {
		t.Fatalf("to_regclass OIDs = %v, want regclass 2205 twice", got)
	}
	rows := catalogRows(t, msgs)
	if len(rows) != 1 || rows[0][0] != "<null>" || rows[0][1] != "wire_state" {
		t.Fatalf("to_regclass rows = %v, want NULL then wire_state", rows)
	}
}

// pg_type's function attributes are regproc (OID 24) with numeric values, not
// the text "-".
func TestPgCatalogRegprocWireType(t *testing.T) {
	c, client := newDataConnection(t)
	msgs := runQuery(t, c, client, simpleQuery(`SELECT typinput, typoutput FROM pg_type WHERE typname = 'int4'`))
	if msg := findError(msgs); msg != nil {
		t.Fatalf("pg_type query failed: %s", errorMessage(msg))
	}
	rd := findRowDescription(t, msgs)
	if got := rowDescriptionOIDs(t, rd.Data); len(got) != 2 || got[0] != 24 || got[1] != 24 {
		t.Fatalf("regproc OIDs = %v, want [24 24]", got)
	}
	rows := catalogRows(t, msgs)
	if len(rows) != 1 || rows[0][0] != "0" || rows[0][1] != "0" {
		t.Fatalf("pg_type regproc values = %v, want 0/0", rows)
	}
}
