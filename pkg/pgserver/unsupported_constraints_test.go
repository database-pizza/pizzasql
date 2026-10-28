package pgserver

import (
	"testing"
)

func TestCheckAndForeignKeyRejectedOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE parent (id INTEGER PRIMARY KEY)"), 0)})

	for _, sql := range []string{
		"CREATE TABLE t1 (id INTEGER PRIMARY KEY, age INTEGER CHECK (age > 0))",
		"CREATE TABLE t2 (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id))",
		"CREATE TABLE t3 (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id))",
	} {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
		if code := errorCode(msgs[0]); code != ErrCodeFeatureNotSupported {
			t.Fatalf("%s: error code = %q, want %q", sql, code, ErrCodeFeatureNotSupported)
		}
	}
}
