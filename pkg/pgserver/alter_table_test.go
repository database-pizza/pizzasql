package pgserver

import (
	"testing"
)

func TestAlterTableAddColumnOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (id) VALUES (1)"), 0)})

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("ALTER TABLE t ADD COLUMN IF NOT EXISTS note TEXT DEFAULT 'later'"), 0)})
	// Idempotent second boot-time migration.
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("ALTER TABLE t ADD COLUMN IF NOT EXISTS note TEXT DEFAULT 'later'"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT id, note FROM t ORDER BY id"), 0)})
	values := dataRowValues(t, msgs[1])
	if len(values) != 2 || values[0] != "1" || values[1] != "<null>" {
		t.Fatalf("existing row after ADD COLUMN = %v, want [1 <null>]", values)
	}

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (id) VALUES (2)"), 0)})
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT note FROM t WHERE id = 2"), 0)})
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "later" {
		t.Fatalf("new row note = %v, want [later]", got)
	}
}

func TestAlterTableDropAndRenameColumnOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY, keep TEXT, dropme TEXT)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("INSERT INTO t (id, keep, dropme) VALUES (1, 'a', 'gone')"), 0)})

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("ALTER TABLE t DROP COLUMN dropme"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("ALTER TABLE t RENAME COLUMN keep TO kept"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT kept FROM t WHERE id = 1"), 0)})
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "a" {
		t.Fatalf("renamed column value = %v, want [a]", got)
	}

	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT dropme FROM t"), 0)})
	if code := errorCode(msgs[0]); code == "" {
		t.Fatalf("expected an error selecting a dropped column, got %v", msgs)
	}
}
