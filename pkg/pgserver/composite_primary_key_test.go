package pgserver

import (
	"testing"
)

// TestCompositePrimaryKeyWireBehavior drives a composite key through the
// simple-query protocol: DDL, duplicate and NOT NULL SQLSTATEs, full-key reads,
// PRAGMA table_info ordinals, and the information_schema key columns.
func TestCompositePrimaryKeyWireBehavior(t *testing.T) {
	c, client := newDataConnection(t)
	query := func(sql string) []*Message {
		return runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
	}

	if msgs := query("CREATE TABLE pairs (a TEXT, b TEXT, v TEXT, PRIMARY KEY (a, b))"); errorCode(msgs[0]) != "" {
		t.Fatalf("CREATE TABLE failed: %s", errorMessage(msgs[0]))
	}
	if msgs := query("INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'one')"); errorCode(msgs[0]) != "" {
		t.Fatalf("INSERT failed: %s", errorMessage(msgs[0]))
	}
	if code := errorCode(query("INSERT INTO pairs (a, b, v) VALUES ('x', 'y', 'two')")[0]); code != ErrCodeUniqueViolation {
		t.Fatalf("duplicate composite key SQLSTATE = %q, want 23505", code)
	}
	if code := errorCode(query("INSERT INTO pairs (a, v) VALUES ('x', 'missing b')")[0]); code != "23502" {
		t.Fatalf("missing key component SQLSTATE = %q, want 23502", code)
	}

	// Only the accepted row survived the two rejected inserts.
	selectRows := query("SELECT v FROM pairs WHERE a = 'x' AND b = 'y'")
	var dataRows []*Message
	for _, msg := range selectRows {
		if msg.Type == MsgDataRow {
			dataRows = append(dataRows, msg)
		}
	}
	if len(dataRows) != 1 {
		t.Fatalf("full-key select returned %d rows, want 1", len(dataRows))
	}
	if values := dataRowValues(t, dataRows[0]); len(values) != 1 || values[0] != "one" {
		t.Fatalf("full-key select values = %v, want [one]", values)
	}

	// PRAGMA table_info reports the pk ordinal, not a bare flag.
	var info [][]string
	for _, msg := range query("PRAGMA table_info(pairs)") {
		if msg.Type == MsgDataRow {
			info = append(info, dataRowValues(t, msg))
		}
	}
	if len(info) != 3 {
		t.Fatalf("table_info rows = %d, want 3", len(info))
	}
	wantPK := []string{"1", "2", "0"}
	for i, row := range info {
		if row[5] != wantPK[i] {
			t.Fatalf("table_info row %d pk = %s, want %s", i, row[5], wantPK[i])
		}
	}

	// KEY_COLUMN_USAGE lists one row per key column with its ordinal.
	var usage [][]string
	for _, msg := range query("SELECT column_name, ordinal_position FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE table_name = 'pairs'") {
		if msg.Type == MsgDataRow {
			usage = append(usage, dataRowValues(t, msg))
		}
	}
	if len(usage) != 2 {
		t.Fatalf("KEY_COLUMN_USAGE rows = %d, want 2", len(usage))
	}
	if usage[0][0] != "a" || usage[0][1] != "1" || usage[1][0] != "b" || usage[1][1] != "2" {
		t.Fatalf("KEY_COLUMN_USAGE = %v, want [[a 1] [b 2]]", usage)
	}
}

func TestCompositePrimaryKeyDDLRejections(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE t1 (a TEXT PRIMARY KEY, b TEXT PRIMARY KEY)",
		"CREATE TABLE t2 (a TEXT PRIMARY KEY, b TEXT, PRIMARY KEY (b))",
		"CREATE TABLE t3 (a TEXT, PRIMARY KEY (a, a))",
	} {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
		if code := errorCode(msgs[0]); code != ErrCodeFeatureNotSupported {
			t.Fatalf("%s SQLSTATE = %q, want 0A000", sql, code)
		}
	}
}
