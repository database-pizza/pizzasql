package pgserver

import (
	"context"
	"strconv"
	"testing"
)

// TestPGXNumericScalarIsFloat8 pins the chosen scalar NUMERIC contract: a
// NUMERIC column and a NUMERIC cast are reported as float8 (701), so pgx scans
// them as float64 and loses digits beyond float64 precision. NUMERIC[] keeps
// exact element text and reports numeric[] (1231).
func TestPGXNumericScalarIsFloat8(t *testing.T) {
	conn := connectPGX(t)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE amounts (id INTEGER PRIMARY KEY, amount NUMERIC, history NUMERIC[])`,
		`INSERT INTO amounts VALUES (1, '12345678901234567890.50', '{12345678901234567890.50,1.10}')`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	rows, err := conn.Query(ctx, `SELECT amount FROM amounts WHERE id = 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if oid := rows.FieldDescriptions()[0].DataTypeOID; oid != 701 {
		t.Fatalf("numeric column OID = %d, want 701 (float8)", oid)
	}
	if !rows.Next() {
		t.Fatal("no row returned")
	}
	var amount float64
	if err := rows.Scan(&amount); err != nil {
		t.Fatalf("scan numeric as float64: %v", err)
	}
	want, err := strconv.ParseFloat("12345678901234567890.50", 64)
	if err != nil {
		t.Fatalf("parse expected value: %v", err)
	}
	if amount != want {
		t.Fatalf("numeric value = %v, want the float64 rounding %v", amount, want)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	rows, err = conn.Query(ctx, `SELECT 1::NUMERIC`)
	if err != nil {
		t.Fatalf("query cast: %v", err)
	}
	defer rows.Close()
	if oid := rows.FieldDescriptions()[0].DataTypeOID; oid != 701 {
		t.Fatalf("numeric cast OID = %d, want 701 (float8)", oid)
	}
	var cast float64
	if !rows.Next() {
		t.Fatal("no row returned for the cast")
	}
	if err := rows.Scan(&cast); err != nil || cast != 1 {
		t.Fatalf("numeric cast scan = %v, %v; want 1", cast, err)
	}
}

// TestPGXNumericArrayKeepsExactText is the other half of the contract: an array
// of NUMERIC stays exact, so pgx reads back every digit and trailing zero.
func TestPGXNumericArrayKeepsExactText(t *testing.T) {
	conn := connectPGX(t)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE amounts (id INTEGER PRIMARY KEY, history NUMERIC[])`,
		`INSERT INTO amounts VALUES (1, '{12345678901234567890.50,1.10}')`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	rows, err := conn.Query(ctx, `SELECT history FROM amounts WHERE id = 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	if oid := rows.FieldDescriptions()[0].DataTypeOID; oid != 1231 {
		t.Fatalf("numeric[] OID = %d, want 1231", oid)
	}
	if !rows.Next() {
		t.Fatal("no row returned")
	}
	var history []string
	if err := rows.Scan(&history); err != nil {
		t.Fatalf("scan numeric[]: %v", err)
	}
	want := []string{"12345678901234567890.50", "1.10"}
	if len(history) != len(want) {
		t.Fatalf("numeric[] = %v, want %v", history, want)
	}
	for i := range want {
		if history[i] != want[i] {
			t.Fatalf("numeric[] = %v, want %v", history, want)
		}
	}
}
