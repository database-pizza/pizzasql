package pgserver

import (
	"bufio"
	"testing"
)

func queryValue(t *testing.T, c *Connection, sql string) ([]string, string) {
	t.Helper()
	client := &countingConn{}
	c.conn = client
	c.writer = bufio.NewWriter(client)
	if err := c.handleMessage(&Message{Type: MsgQuery, Data: append([]byte(sql), 0)}); err != nil {
		t.Fatal(err)
	}
	messages := readAllMessages(t, client.bytes())
	if len(messages) > 0 && messages[0].Type == MsgErrorResponse {
		return nil, errorCode(messages[0])
	}
	for _, message := range messages {
		if message.Type == MsgDataRow {
			return dataRowValues(t, message), ""
		}
	}
	return nil, ""
}

func TestSmallPostgreSQLCompatibilityFunctions(t *testing.T) {
	c, _ := newDataConnection(t)
	if _, code := queryValue(t, c, "CREATE TABLE agg (v TEXT)"); code != "" {
		t.Fatal(code)
	}
	if _, code := queryValue(t, c, "INSERT INTO agg VALUES ('x'),('y'),('x')"); code != "" {
		t.Fatal(code)
	}
	for _, tc := range []struct{ sql, want string }{
		{"SELECT concat_ws('-', 'a', NULL, 'b')", "a-b"},
		{"SELECT concat_ws(NULL, 'a', 'b')", "<null>"},
		{"SELECT concat_ws('-', 'a')", "a"},
		{"SELECT json_array_length('[1,2,3]')", "3"},
		{"SELECT json_array_length('{\"a\":1}')", "0"},
		{"SELECT json_array_length('[1,[2,3]]', '$[1]')", "2"},
		{"SELECT json_array_length('{\"a\":[1,2]}', '$.a')", "2"},
		{"SELECT pg_backend_pid() = pg_backend_pid()", "true"},
		{"SELECT string_agg(v, ',') FROM agg", "x,y,x"},
		{"SELECT string_agg(DISTINCT v, ',') FROM agg", "x,y"},
	} {
		values, code := queryValue(t, c, tc.sql)
		if code != "" {
			t.Fatalf("%s: SQLSTATE %s", tc.sql, code)
		}
		if len(values) != 1 || values[0] != tc.want {
			t.Fatalf("%s: got %v want %s", tc.sql, values, tc.want)
		}
	}
	if values, code := queryValue(t, c, "SELECT json_array_length('not json')"); code == "" || len(values) != 0 {
		t.Fatalf("malformed JSON accepted: %v %s", values, code)
	}
	if values, code := queryValue(t, c, "SELECT string_agg('x', NULL)"); code != "" || len(values) != 1 || values[0] != "<null>" {
		t.Fatalf("NULL delimiter: %v %s", values, code)
	}
	if values, code := queryValue(t, c, "SELECT string_agg(NULL, ',')"); code != "" || len(values) != 1 || values[0] != "<null>" {
		t.Fatalf("NULL input: %v %s", values, code)
	}
}
