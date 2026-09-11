package pgserver

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

func TestGetOIDForTypeDatetime(t *testing.T) {
	c := &Connection{}
	cases := map[string]int32{
		"DATETIME":  1184,
		"TIMESTAMP": 1184,
		"DATE":      1082,
		"BIGINT":    20,
		"INTEGER":   23,
		"INT":       23,
		"TEXT":      25,
		"UUID":      25,
		"BOOLEAN":   16,
	}
	for name, want := range cases {
		if got := c.getOIDForType(name); got != want {
			t.Errorf("getOIDForType(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestSendRowDescriptionTimestampOID(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	if err := c.sendRowDescription([]string{"created_at", "id"}, []string{"TIMESTAMP", "BIGINT"}); err != nil {
		t.Fatal(err)
	}
	if err := c.sendReadyForQuery(); err != nil {
		t.Fatal(err)
	}
	msgs := readAllMessages(t, cc.bytes())
	rd := findRowDescription(t, msgs)
	oids := rowDescriptionOIDs(t, rd.Data)
	if len(oids) != 2 {
		t.Fatalf("got %d columns, want 2", len(oids))
	}
	if oids[0] != 1184 {
		t.Errorf("timestamp OID = %d, want 1184", oids[0])
	}
	if oids[1] != 20 {
		t.Errorf("bigint OID = %d, want 20", oids[1])
	}
}

func TestSendResultEmptyProjectionSendsRowDescription(t *testing.T) {
	c, cc := newCountingTestConnection(t)
	result := executor.NewResult("SELECT")
	result.AddColumnWithType("id", "BIGINT")
	result.AddColumnWithType("created_at", "DATETIME")
	if err := c.sendResult(result, &parser.SelectStmt{}); err != nil {
		t.Fatal(err)
	}
	if err := c.sendReadyForQuery(); err != nil {
		t.Fatal(err)
	}
	msgs := readAllMessages(t, cc.bytes())
	rd := findRowDescription(t, msgs)
	oids := rowDescriptionOIDs(t, rd.Data)
	if len(oids) != 2 || oids[0] != 20 || oids[1] != 1184 {
		t.Fatalf("oids = %v, want [20 1184]", oids)
	}
}

func findRowDescription(t *testing.T, msgs []*Message) *Message {
	t.Helper()
	for _, m := range msgs {
		if m.Type == MsgRowDescription {
			return m
		}
	}
	t.Fatal("no RowDescription message found")
	return nil
}

func rowDescriptionOIDs(t *testing.T, data []byte) []int32 {
	t.Helper()
	if len(data) < 2 {
		t.Fatalf("row description too short")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	pos := 2
	oids := make([]int32, 0, count)
	for i := 0; i < count; i++ {
		end := bytes.IndexByte(data[pos:], 0)
		if end < 0 {
			t.Fatalf("unterminated column name")
		}
		pos += end + 1 // name
		pos += 4       // table OID
		pos += 2       // column attribute number
		if pos+4 > len(data) {
			t.Fatalf("truncated row description")
		}
		oids = append(oids, int32(binary.BigEndian.Uint32(data[pos:pos+4])))
		pos += 4 // type OID
		pos += 2 // type size
		pos += 4 // type modifier
		pos += 2 // format code
	}
	return oids
}
