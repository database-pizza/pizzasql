package pgserver

import "testing"

// TestLiteralCastResultOIDs pins the reported type of a cast whose operand is a
// constant. evalCastExpr carries int64/float64 and does not range-check, so a
// declared width narrower than the carrier is advertised only when the constant
// fits it; otherwise the carrier type (int8/float8) is reported.
func TestLiteralCastResultOIDs(t *testing.T) {
	c, client := newDataConnection(t)

	cases := []struct {
		sql   string
		want  int32
		value string
	}{
		{"SELECT 1::SMALLINT", 21, "1"},         // int2
		{"SELECT CAST(1 AS SMALLINT)", 21, "1"}, // int2
		{"SELECT 32767::SMALLINT", 21, "32767"}, // int2
		{"SELECT -1::SMALLINT", 21, "-1"},       // int2
		{"SELECT 32768::SMALLINT", 20, "32768"}, // int8 carrier
		{"SELECT 1::INTEGER", 23, "1"},          // int4
		{"SELECT 1::INT", 23, "1"},              // int4
		{"SELECT 2147483647::INTEGER", 23, "2147483647"},
		{"SELECT 2147483648::INTEGER", 20, "2147483648"}, // int8 carrier
		{"SELECT 1::BIGINT", 20, "1"},                    // int8
		{"SELECT 1.5::REAL", 700, "1.5"},                 // float4
		{"SELECT CAST(1 AS REAL)", 700, "1"},             // float4
		{"SELECT 3.5e38::REAL", 701, "3.5e+38"},          // float8 carrier
		{"SELECT 1.5::DOUBLE", 701, "1.5"},               // float8
		{"SELECT 1.5::DOUBLE PRECISION", 701, "1.5"},     // float8
		{"SELECT 1::NUMERIC", 701, "1"},                  // float8 carrier
		{"SELECT 1.5::DECIMAL", 701, "1.5"},              // float8 carrier
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			if errMsg := findError(msgs); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != tc.want {
				t.Fatalf("%s: OIDs = %v, want [%d]", tc.sql, oids, tc.want)
			}
			if row := firstDataRow(t, msgs); len(row) != 1 || row[0] != tc.value {
				t.Fatalf("%s: row = %v, want [%s]", tc.sql, row, tc.value)
			}
		})
	}
}

// TestNumericResultMetadataIsFloat8 pins the scalar NUMERIC contract from both
// directions: a declared NUMERIC/DECIMAL column and a NUMERIC cast report
// float8 (701) because scalars are float64-backed, while NUMERIC[] keeps exact
// element text and reports numeric[] (1231).
func TestNumericResultMetadataIsFloat8(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		`CREATE TABLE amounts (id INTEGER PRIMARY KEY, price NUMERIC, discount DECIMAL, history NUMERIC[])`,
		`INSERT INTO amounts VALUES (1, '12345678901234567890.50', '1.10', '{12345678901234567890.50,1.10}')`,
	} {
		if msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}); findError(msgs) != nil {
			t.Fatalf("%s: %s", sql, errorCode(findError(msgs)))
		}
	}

	cases := []struct {
		sql   string
		want  int32
		value string
	}{
		{"SELECT price FROM amounts", 701, "12345678901234567890.50"},
		{"SELECT discount FROM amounts", 701, "1.10"},
		{"SELECT history FROM amounts", 1231, "{12345678901234567890.50,1.10}"},
		{"SELECT price::NUMERIC FROM amounts", 701, "12345678901234567890.50"},
		{"SELECT CAST(discount AS DECIMAL) FROM amounts", 701, "1.10"},
		{"SELECT price + 0 FROM amounts", 701, "1.2345678901234567e+19"},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			if errMsg := findError(msgs); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != tc.want {
				t.Fatalf("%s: OIDs = %v, want [%d]", tc.sql, oids, tc.want)
			}
			if row := firstDataRow(t, msgs); len(row) != 1 || row[0] != tc.value {
				t.Fatalf("%s: row = %v, want [%s]", tc.sql, row, tc.value)
			}
		})
	}
}

// TestCastOverColumnKeepsCarrierOID guards the other half of the literal-cast
// rule: the value of a column is not proven to fit, so it keeps the carrier
// type even when the column's declared type is narrower.
func TestCastOverColumnKeepsCarrierOID(t *testing.T) {
	c, client := newDataConnection(t)
	for _, sql := range []string{
		`CREATE TABLE wide (id INTEGER PRIMARY KEY, big BIGINT, score DOUBLE)`,
		`INSERT INTO wide VALUES (1, 40000, 3.5e38)`,
	} {
		if msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}); findError(msgs) != nil {
			t.Fatalf("%s: %s", sql, errorCode(findError(msgs)))
		}
	}

	for _, tc := range []struct {
		sql  string
		want int32
	}{
		{`SELECT big::SMALLINT FROM wide`, 20},
		{`SELECT big::INTEGER FROM wide`, 20},
		{`SELECT score::REAL FROM wide`, 701},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			if errMsg := findError(msgs); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != tc.want {
				t.Fatalf("%s: OIDs = %v, want [%d]", tc.sql, oids, tc.want)
			}
		})
	}
}

// TestExactCastOIDsSurviveBinaryResults checks that a binary result requests the
// exact width the RowDescription advertises: int2 is two bytes, float4 is four,
// and a scalar NUMERIC cast is a float8.
func TestExactCastOIDsSurviveBinaryResults(t *testing.T) {
	cases := []struct {
		sql  string
		want int32
		data []byte
	}{
		{"SELECT 1::SMALLINT", 21, []byte{0, 1}},
		{"SELECT 1.5::REAL", 700, []byte{0x3f, 0xc0, 0, 0}},
		{"SELECT 1::NUMERIC", 701, []byte{0x3f, 0xf0, 0, 0, 0, 0, 0, 0}},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			c, conn := extendedResultConnection(t)
			sendExtended(t, c,
				parseMessage("s", tc.sql),
				bindResultMessage("p", "s", nil, []int16{1}),
				executeMessage("p"),
				&Message{Type: MsgSync},
			)
			msgs := readAllMessages(t, conn.bytes())
			if errMsg := findError(msgs); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != tc.want {
				t.Fatalf("%s: OIDs = %v, want [%d]", tc.sql, oids, tc.want)
			}
			rows := dataRowsRaw(t, msgs)
			if len(rows) != 1 || len(rows[0]) != 1 {
				t.Fatalf("%s: rows = %v", tc.sql, rows)
			}
			assertBytes(t, tc.sql, rows[0][0], tc.data)
		})
	}
}

// TestArrayConstructorOfExactCast keeps the array path consistent: an exact cast
// used as an ARRAY[...] element maps back to its carrier array OID instead of
// falling through to text[].
func TestArrayConstructorOfExactCast(t *testing.T) {
	c, client := newDataConnection(t)
	cases := []struct {
		sql   string
		want  int32
		value string
	}{
		{"SELECT ARRAY[1::SMALLINT]", 1016, "{1}"},
		{"SELECT ARRAY[1.5::REAL]", 1021, "{1.5}"},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			msgs := resultMessages(runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(tc.sql), 0)}))
			if errMsg := findError(msgs); errMsg != nil {
				t.Fatalf("%s failed: %s", tc.sql, errorCode(errMsg))
			}
			oids := rowDescriptionOIDs(t, findRowDescription(t, msgs).Data)
			if len(oids) != 1 || oids[0] != tc.want {
				t.Fatalf("%s: OIDs = %v, want [%d]", tc.sql, oids, tc.want)
			}
			if row := firstDataRow(t, msgs); len(row) != 1 || row[0] != tc.value {
				t.Fatalf("%s: row = %v, want [%s]", tc.sql, row, tc.value)
			}
		})
	}
}
