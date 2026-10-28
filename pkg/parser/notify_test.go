package parser

import (
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
)

func parseNotifySQL(t *testing.T, sql string) (Statement, error) {
	t.Helper()
	return New(lexer.New(sql)).Parse()
}

func TestParseListen(t *testing.T) {
	stmt, err := parseNotifySQL(t, `LISTEN orders`)
	if err != nil {
		t.Fatal(err)
	}
	listen, ok := stmt.(*ListenStmt)
	if !ok || listen.Channel != "orders" {
		t.Fatalf("unexpected statement: %#v", stmt)
	}

	quoted, err := parseNotifySQL(t, `LISTEN "Mixed Case"`)
	if err != nil {
		t.Fatal(err)
	}
	if quoted.(*ListenStmt).Channel != "Mixed Case" {
		t.Fatalf("quoted channel = %q", quoted.(*ListenStmt).Channel)
	}
}

func TestParseUnlisten(t *testing.T) {
	stmt, err := parseNotifySQL(t, `UNLISTEN orders`)
	if err != nil {
		t.Fatal(err)
	}
	unlisten := stmt.(*UnlistenStmt)
	if unlisten.All || unlisten.Channel != "orders" {
		t.Fatalf("unexpected unlisten: %#v", unlisten)
	}

	all, err := parseNotifySQL(t, `UNLISTEN *`)
	if err != nil {
		t.Fatal(err)
	}
	if !all.(*UnlistenStmt).All {
		t.Fatal("expected UNLISTEN *")
	}
}

func TestParseNotify(t *testing.T) {
	stmt, err := parseNotifySQL(t, `NOTIFY orders, 'created'`)
	if err != nil {
		t.Fatal(err)
	}
	notify := stmt.(*NotifyStmt)
	if notify.Channel != "orders" || notify.Payload != "created" {
		t.Fatalf("unexpected notify: %#v", notify)
	}

	empty, err := parseNotifySQL(t, `NOTIFY orders`)
	if err != nil {
		t.Fatal(err)
	}
	if empty.(*NotifyStmt).Payload != "" {
		t.Fatalf("payload = %q, want empty", empty.(*NotifyStmt).Payload)
	}

	escaped, err := parseNotifySQL(t, `NOTIFY orders, 'it''s here'`)
	if err != nil {
		t.Fatal(err)
	}
	if escaped.(*NotifyStmt).Payload != "it's here" {
		t.Fatalf("payload = %q", escaped.(*NotifyStmt).Payload)
	}
}

func TestParseNotifyPayloadLimit(t *testing.T) {
	payload := strings.Repeat("a", 8000)
	if _, err := parseNotifySQL(t, `NOTIFY ch, '`+payload+`'`); err != nil {
		t.Fatalf("8000-byte payload rejected: %v", err)
	}
	if _, err := parseNotifySQL(t, `NOTIFY ch, '`+payload+`a'`); err == nil {
		t.Fatal("expected 8001-byte payload to be rejected")
	}
}

func TestParseChannelNameLimit(t *testing.T) {
	channel := strings.Repeat("a", 63)
	if _, err := parseNotifySQL(t, `LISTEN `+channel); err != nil {
		t.Fatalf("63-byte channel rejected: %v", err)
	}
	if _, err := parseNotifySQL(t, `LISTEN `+channel+`a`); err == nil {
		t.Fatal("expected 64-byte channel to be rejected")
	}
}

func TestParseNotifyInvalid(t *testing.T) {
	for _, sql := range []string{
		`LISTEN`,
		`UNLISTEN`,
		`NOTIFY ch,`,
		`NOTIFY ch, 42`,
		`NOTIFY ch 'payload'`,
		`LISTEN 42`,
	} {
		if _, err := parseNotifySQL(t, sql); err == nil {
			t.Errorf("expected %q to fail", sql)
		}
	}
}

func TestNotifyIsNotReservedAsColumnName(t *testing.T) {
	stmt, err := parseNotifySQL(t, `SELECT notify, listen FROM events`)
	if err != nil {
		t.Fatalf("contextual keywords should remain usable as columns: %v", err)
	}
	sel := stmt.(*SelectStmt)
	if len(sel.Columns) != 2 {
		t.Fatalf("columns = %d, want 2", len(sel.Columns))
	}
}
