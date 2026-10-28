package pgserver

import (
	"bytes"
	"strings"
	"testing"
)

// jsonErrorField extracts a named field from an ErrorResponse.
func jsonErrorField(t *testing.T, msg *Message, want byte) string {
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

// The supported JSON contract is TEXT storage plus the JSON1 function surface.
// JSON/JSONB type names are accepted as text-affinity aliases; there is no
// native binary jsonb type or ->> operator.
func TestJSONTextContractOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE settings (id INTEGER PRIMARY KEY, body TEXT)"), 0)})

	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`INSERT INTO settings (id, body) VALUES (1, '{"theme":"dark","widgets":[1,2]}')`), 0)})

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT json_extract(body, '$.theme'), json_type(body), json_valid(body) FROM settings WHERE id = 1`), 0)})
	values := dataRowValues(t, msgs[1])
	if len(values) != 3 || values[0] != "dark" || values[1] != "object" || values[2] != "1" {
		t.Fatalf("json round trip = %v, want [dark object 1]", values)
	}

	// Partial update through json_set, without rewriting the whole document.
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`UPDATE settings SET body = json_set(body, '$.theme', 'light') WHERE id = 1`), 0)})
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT json_extract(body, '$.theme'), json_extract(body, '$.widgets[1]') FROM settings WHERE id = 1`), 0)})
	values = dataRowValues(t, msgs[1])
	if len(values) != 2 || values[0] != "light" || values[1] != "2" {
		t.Fatalf("json partial update = %v, want [light 2]", values)
	}

	// JSON and JSONB column type names are text-affinity aliases.
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte("CREATE TABLE docs (id INTEGER PRIMARY KEY, body JSON)"), 0)})
	runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`INSERT INTO docs (id, body) VALUES (1, '{"a":1}')`), 0)})
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT json_extract(body, '$.a') FROM docs`), 0)})
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "1" {
		t.Fatalf("JSON column extract = %v, want [1]", got)
	}
}

func TestJSONInvalidInputOverSimpleQuery(t *testing.T) {
	c, client := newDataConnection(t)

	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT json_extract('not json', '$.a')`), 0)})
	if len(msgs) != 2 || msgs[0].Type != MsgErrorResponse {
		t.Fatalf("expected ErrorResponse+ReadyForQuery, got %v", msgs)
	}
	if msg := jsonErrorField(t, msgs[0], ErrorFieldMessage); !strings.Contains(msg, "malformed JSON") {
		t.Fatalf("error message = %q, want a malformed-JSON error", msg)
	}

	// json_valid is the non-throwing way to probe a value.
	msgs = runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT json_valid('not json')`), 0)})
	if got := dataRowValues(t, msgs[1]); len(got) != 1 || got[0] != "0" {
		t.Fatalf("json_valid = %v, want [0]", got)
	}
}

func TestJSONArrowOperatorUnsupported(t *testing.T) {
	c, client := newDataConnection(t)
	msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(`SELECT '{"a":1}' -> '$.a'`), 0)})
	if len(msgs) == 0 || msgs[0].Type != MsgErrorResponse {
		t.Fatalf("expected a parse error for the -> operator, got %v", msgs)
	}
}
