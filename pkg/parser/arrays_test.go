package parser

import (
	"errors"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
)

func TestArrayParametersAndAnyPredicatesRejected(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t WHERE id = ANY('{1,2,3}')",
		"SELECT * FROM t WHERE id <> ANY('{1,2,3}')",
		"SELECT * FROM t WHERE id = ALL('{1,2,3}')",
		"SELECT * FROM t WHERE id = ANY(ARRAY[1,2])",
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := New(lexer.New(sql)).Parse()
			if !errors.Is(err, pgerror.ErrFeatureNotSupported) {
				t.Fatalf("error = %v, want ErrFeatureNotSupported", err)
			}
		})
	}
}

func TestAnyAsOrdinaryColumnStillParses(t *testing.T) {
	// ANY is only a predicate keyword when followed by "(". A column literally
	// named any must keep working.
	if _, err := New(lexer.New("SELECT any FROM t")).Parse(); err != nil {
		t.Fatalf("parse: %v", err)
	}
}
