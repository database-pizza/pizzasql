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

func TestLockingClauseRejectedOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE t (id INTEGER PRIMARY KEY)"), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("SELECT * FROM t FOR UPDATE"), 0)})
	if code := errorCode(msgs[0]); code != ErrCodeFeatureNotSupported {
		t.Fatalf("error code = %q, want %q", code, ErrCodeFeatureNotSupported)
	}
	if message := errorField(t, msgs[0], ErrorFieldMessage); !bytes.Contains([]byte(message), []byte("row locking")) {
		t.Fatalf("error message = %q, want a row-locking explanation", message)
	}
}
