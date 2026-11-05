package parser

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

func TestSelectLockingClauseParses(t *testing.T) {
	cases := []struct {
		sql      string
		strength LockStrength
		wait     LockWait
	}{
		{"SELECT * FROM t FOR UPDATE", LockUpdate, LockWaitBlock},
		{"SELECT * FROM t WHERE id = 1 FOR UPDATE NOWAIT", LockUpdate, LockWaitNoWait},
		{"SELECT * FROM t FOR SHARE", LockShare, LockWaitBlock},
		{"SELECT id FROM t ORDER BY id LIMIT 10 FOR UPDATE SKIP LOCKED", LockUpdate, LockWaitSkipLocked},
		{"SELECT * FROM t FOR UPDATE OF t SKIP LOCKED", 0, 0},
	}
	for i, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := New(lexer.New(tc.sql)).Parse()
			if i == 4 {
				if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
					t.Fatalf("error = %v, want ErrFeatureNotSupported", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sel, ok := stmt.(*SelectStmt)
			if !ok || sel.Locking == nil {
				t.Fatalf("locking clause missing: %#v", stmt)
			}
			if sel.Locking.Strength != tc.strength || sel.Locking.Wait != tc.wait {
				t.Fatalf("locking = %#v, want %v/%v", sel.Locking, tc.strength, tc.wait)
			}
		})
	}
}

func TestSelectLockingUnsupportedForms(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t FOR NO KEY UPDATE",
		"SELECT * FROM t FOR KEY SHARE",
		"SELECT id FROM t UNION SELECT id FROM u FOR UPDATE",
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := New(lexer.New(sql)).Parse()
			if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
				t.Fatalf("error = %v, want ErrFeatureNotSupported", err)
			}
		})
	}
}

func TestSelectWithoutLockingClauseStillParses(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t",
		"SELECT * FROM t WHERE id = 1",
		"SELECT * FROM t ORDER BY id LIMIT 1",
	} {
		if _, err := New(lexer.New(sql)).Parse(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}
