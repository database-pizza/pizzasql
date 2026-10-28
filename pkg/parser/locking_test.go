package parser

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

func TestSelectLockingClauseRejected(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t WHERE id = 1 FOR UPDATE NOWAIT",
		"SELECT * FROM t FOR SHARE",
		"SELECT * FROM t FOR NO KEY UPDATE SKIP LOCKED",
		"SELECT id FROM t ORDER BY id LIMIT 10 FOR UPDATE",
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
