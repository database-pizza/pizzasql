package pgserver

import (
	"testing"
)

// RowDescription for the hidden rowid alias must report an integer OID, not
// TEXT, because the value is a signed 64-bit integer.
func TestRowIDWireTypeIsBigint(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id TEXT PRIMARY KEY)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (id) VALUES ('a')"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT _rowid_ FROM t"), 0)})
	if msgs[0].Type != MsgRowDescription {
		t.Fatalf("first message = %c, want RowDescription", msgs[0].Type)
	}
	oids := rowDescriptionOIDs(t, msgs[0].Data)
	if len(oids) != 1 || oids[0] != 20 { // INT8OID
		t.Fatalf("rowid OIDs = %v, want [20]", oids)
	}
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "1" {
		t.Fatalf("rowid value = %v, want [1]", got)
	}
}

// A keyset cursor built from _rowid_ must paginate without repeating or
// skipping rows, including rows inserted after the first page is read.
func TestRowIDKeysetPaginationOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE events (id TEXT PRIMARY KEY, payload TEXT)"), 0)})
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO events (id, payload) VALUES ('"+id+"', 'x')"), 0)})
	}

	page := func(cursor string) []string {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT _rowid_ FROM events WHERE _rowid_ > "+cursor+" ORDER BY _rowid_ LIMIT 2"), 0)})
		if len(msgs) < 2 || msgs[0].Type != MsgRowDescription {
			t.Fatalf("unexpected page response: %v", msgs)
		}
		var ids []string
		for _, msg := range msgs[1:] {
			if msg.Type != MsgDataRow {
				break
			}
			ids = append(ids, dataRowValues(t, msg)[0])
		}
		return ids
	}

	first := page("0")
	if len(first) != 2 || first[0] != "1" || first[1] != "2" {
		t.Fatalf("first page = %v, want [1 2]", first)
	}
	// A late insert must not move the cursor backwards or duplicate page 1.
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO events (id, payload) VALUES ('late', 'x')"), 0)})

	second := page(first[1])
	if len(second) != 2 || second[0] != "3" || second[1] != "4" {
		t.Fatalf("second page = %v, want [3 4]", second)
	}
	third := page(second[1])
	if len(third) != 2 || third[0] != "5" || third[1] != "6" {
		t.Fatalf("third page = %v, want [5 6] (late row appended)", third)
	}
	if fourth := page(third[1]); len(fourth) != 0 {
		t.Fatalf("final page = %v, want empty", fourth)
	}
}

// INSERT ... RETURNING _rowid_ must expose the assigned cursor value.
func TestRowIDReturningOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id TEXT PRIMARY KEY)"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (id) VALUES ('a') RETURNING _rowid_"), 0)})
	if len(msgs) < 3 {
		t.Fatalf("expected RowDescription+DataRow+CommandComplete, got %v", msgs)
	}
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "1" {
		t.Fatalf("returned rowid = %v, want [1]", got)
	}
}
