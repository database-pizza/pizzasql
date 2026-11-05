package pgserver

import (
	"bytes"
	"testing"
)

// errorField extracts a named field from an ErrorResponse.
func errorField(t *testing.T, msg *Message, want byte) string {
	t.Helper()
	if msg == nil || msg.Type != MsgErrorResponse {
		t.Fatalf("message type = %v, want ErrorResponse", msg)
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
		if field == want {
			return value
		}
		i += end + 1
	}
	return ""
}

func TestLockingClauseSupportedShapeOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY)"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT * FROM t FOR UPDATE"), 0)})
	if code := errorCode(msgs[0]); code != "" {
		t.Fatalf("locking query error = %q", code)
	}
}

func TestLockingClauseRejectsUnsupportedShapes(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY)",
		"CREATE TABLE u (id INTEGER PRIMARY KEY)",
	} {
		runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
	}
	for _, sql := range []string{
		"SELECT * FROM t a JOIN u b ON a.id = b.id FOR UPDATE",
		"SELECT COUNT(*) FROM t FOR UPDATE",
		"SELECT DISTINCT id FROM t FOR UPDATE",
		"SELECT id FROM t UNION SELECT id FROM u FOR UPDATE",
	} {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
		if code := errorCode(msgs[0]); code != ErrCodeFeatureNotSupported {
			t.Fatalf("%s: error code = %q, want %q", sql, code, ErrCodeFeatureNotSupported)
		}
	}
}
