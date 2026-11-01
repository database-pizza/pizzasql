package pgserver

import (
	"strings"
	"testing"
)

func TestBindQuerySkipsCommentsAndQuotedRegions(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"SELECT $1 /* ' $99 */ + $2", "SELECT 1 /* ' $99 */ + 2"},
		{"SELECT $1 -- '$99\n + $2", "SELECT 1 -- '$99\n + 2"},
		{"SELECT $1 -- $99", "SELECT 1 -- $99"},
		{"SELECT '$99''$88', \"$77\", `$66`, [$55], $1", "SELECT '$99''$88', \"$77\", `$66`, [$55], 1"},
		{"SELECT '$1 /* no comment */', $2", "SELECT '$1 /* no comment */', 2"},
	} {
		got, err := bindQuery(tc.query, []boundParameter{{value: []byte("1")}, {value: []byte("2")}})
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q err=%v want %q", tc.query, got, err, tc.want)
		}
	}
	if _, err := bindQuery("SELECT $99999999999999999999999999", nil); err == nil {
		t.Fatal("overflowed parameter number accepted")
	}
}

func TestCommentedPreparedQueryExecutesBothParameters(t *testing.T) {
	c, client := newDataConnection(t)
	c.statements[""] = &preparedStatement{query: "SELECT $1 /* ' $99 */ + $2", paramOIDs: []int32{20, 20}}
	bind := NewMessageBuilder()
	bind.WriteString("")
	bind.WriteString("")
	bind.WriteInt16(0)
	bind.WriteInt16(2)
	for _, value := range []string{"1", "2"} {
		bind.WriteInt32(int32(len(value)))
		bind.WriteBytes([]byte(value))
	}
	bind.WriteInt16(0)
	if err := c.handleBind(&Message{Type: MsgBind, Data: bind.Bytes()}); err != nil {
		t.Fatal(err)
	}
	portal := c.portals[""]
	if portal == nil {
		t.Fatal("Bind failed")
	}
	messages := runExtendedExecution(t, c, client, portal.query)
	found := false
	for _, message := range messages {
		if message.Type == MsgErrorResponse {
			t.Fatalf("bound execution failed: %s", errorCode(message))
		}
		if message.Type == MsgDataRow {
			values := dataRowValues(t, message)
			if len(values) != 1 || values[0] != "3" {
				t.Fatal(values)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no result row")
	}
}

func TestSystemQueryMatchingDoesNotInterceptLiteralsOrProjections(t *testing.T) {
	c, client := newDataConnection(t)
	for _, literal := range []string{"SELECT VERSION()", "SELECT CURRENT_USER", "SHOW SERVER_VERSION", "FROM PG_TABLES", "FROM INFORMATION_SCHEMA.TABLES"} {
		messages := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte("SELECT '" + literal + "'\x00")})
		found := false
		for _, message := range messages {
			if message.Type == MsgErrorResponse {
				t.Fatalf("literal %q failed: %s", literal, errorCode(message))
			}
			if message.Type == MsgDataRow {
				values := dataRowValues(t, message)
				if len(values) != 1 || values[0] != literal {
					t.Fatalf("literal %q intercepted: %v", literal, values)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("literal %q produced no row", literal)
		}
	}
	for _, sql := range []string{"SELECT version() + 1", "SELECT current_user, 1", "SELECT 1 /* SELECT VERSION() */", "SHOW other_server_version"} {
		if kind := systemQueryKind(sql); kind != "" {
			t.Fatalf("%q intercepted as %s", sql, kind)
		}
	}
	for _, sql := range []string{"select version ( ) /* ; SELECT 2 */;", "-- initialization\nSELECT version()"} {
		messages := runQuery(t, c, client, &Message{Type: MsgQuery, Data: []byte(sql + "\x00")})
		if values := dataRowValues(t, messages[1]); !strings.Contains(values[0], "PizzaSQL") {
			t.Fatal(values)
		}
	}
}

func TestSingleStatementMatchingIgnoresCommentSemicolons(t *testing.T) {
	for _, sql := range []string{"SELECT ';'", "SELECT 1 /* ; ' */; -- ; SELECT 2", "SELECT 1; ;"} {
		if !isSingleStatementSQL(sql) {
			t.Fatalf("single statement rejected: %q", sql)
		}
	}
	if isSingleStatementSQL("SELECT 1; /* ; */ SELECT 2") {
		t.Fatal("batch accepted as one statement")
	}
}
