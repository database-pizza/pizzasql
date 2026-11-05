package pgserver

import (
	"context"
	"testing"
)

// A bound parameter is inlined as a literal, but that literal is not a
// user-written constant: casts over parameters must keep the carrier OID so
// result metadata does not depend on the bound value.
func TestParameterCastKeepsCarrierOID(t *testing.T) {
	conn := connectPGX(t)
	defer conn.Close(context.Background())

	cases := []struct {
		query string
		arg   any
		want  uint32
	}{
		{"SELECT $1::SMALLINT", int16(1), 20},
		{"SELECT $1::SMALLINT", int64(40000), 20},
		{"SELECT $1::INTEGER", int32(1), 20},
		{"SELECT $1::REAL", float32(1.5), 701},
	}
	for _, tc := range cases {
		rows, err := conn.Query(context.Background(), tc.query, tc.arg)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		fields := rows.FieldDescriptions()
		if len(fields) != 1 || fields[0].DataTypeOID != tc.want {
			t.Fatalf("%s: got %v, want OID %d", tc.query, fields, tc.want)
		}
		rows.Close()
	}

	// User-written literals still report their exact width.
	rows, err := conn.Query(context.Background(), "SELECT 1::SMALLINT")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.FieldDescriptions()[0].DataTypeOID; got != 21 {
		t.Fatalf("literal SMALLINT cast OID = %d, want 21", got)
	}
	rows.Close()
}
