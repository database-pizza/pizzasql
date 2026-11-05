package analyzer

import (
	"testing"
)

// jsonbCatalog adds a table with a JSONB column to the shared test catalog.
func jsonbCatalog() *Catalog {
	catalog := setupCatalog()
	catalog.CreateTable(&TableInfo{
		Name: "docs",
		Columns: []ColumnInfo{
			{Name: "id", Type: TypeInteger, PrimaryKey: true},
			{Name: "doc", Type: TypeJSONB, Nullable: true},
			{Name: "body", Type: TypeText, Nullable: true},
		},
	})
	return catalog
}

func TestTypeFromNameJSONB(t *testing.T) {
	if got := TypeFromName("JSONB"); got != TypeJSONB {
		t.Fatalf("TypeFromName(JSONB) = %v, want JSONB", got)
	}
	// JSON stays a text-backed alias and must not be promoted to native jsonb.
	if got := TypeFromName("JSON"); got == TypeJSONB {
		t.Fatal("TypeFromName(JSON) = JSONB, want the existing text-backed affinity")
	}
	if !TypeJSONB.IsComparable(TypeJSONB) || !TypeJSONB.IsComparable(TypeText) {
		t.Fatal("jsonb must compare with jsonb and text")
	}
	if TypeJSONB.IsComparable(TypeInteger) {
		t.Fatal("jsonb must not compare with an integer")
	}
	if got := CommonType(TypeJSONB, TypeText); got != TypeJSONB {
		t.Fatalf("CommonType(JSONB, TEXT) = %v, want JSONB", got)
	}
}

func TestAnalyzeJSONBExpressions(t *testing.T) {
	a := New(jsonbCatalog())
	for _, sql := range []string{
		`SELECT doc -> 'a', doc ->> 'a' FROM docs`,
		`SELECT doc #> '{a}', doc #>> '{a}', doc #- '{a}' FROM docs`,
		`SELECT doc @> '{}'::jsonb, doc <@ '{}'::jsonb FROM docs`,
		`SELECT doc ? 'a', doc ?| '{a}', doc ?& '{a}' FROM docs`,
		`SELECT doc || '{}'::jsonb, doc - 'a', doc - 1 FROM docs`,
		`SELECT '{}'::jsonb, CAST('{}' AS JSONB), to_jsonb(doc) FROM docs`,
		`SELECT jsonb_typeof(doc), jsonb_array_length(doc), jsonb_pretty(doc) FROM docs`,
		`SELECT jsonb_build_object('a', 1), jsonb_build_array(1, 'x') FROM docs`,
		`SELECT jsonb_set(doc, '{a}', '1'), jsonb_insert(doc, '{a}', '1') FROM docs`,
		`SELECT * FROM jsonb_each('{"a":1}')`,
		`SELECT value FROM jsonb_array_elements('[1]')`,
		`SELECT jsonb_object_keys FROM jsonb_object_keys('{"a":1}')`,
		`SELECT CASE WHEN id = 1 THEN doc ELSE '{}'::jsonb END FROM docs`,
		`SELECT COALESCE(doc, '{}'::jsonb) FROM docs`,
	} {
		if err := analyze(t, a, sql); err != nil {
			t.Errorf("%s: unexpected error: %v", sql, err)
		}
	}

	for _, sql := range []string{
		`SELECT doc = 5 FROM docs`,
		`SELECT doc + 1 FROM docs`,
		`SELECT 1 - doc FROM docs`,
		`SELECT doc & 1 FROM docs`,
		`SELECT doc || 1 FROM docs`,
		`SELECT doc - doc FROM docs`,
	} {
		err := analyze(t, a, sql)
		if err == nil {
			t.Errorf("%s: expected a type error", sql)
			continue
		}
		if analysisErr, ok := err.(*AnalysisError); !ok || analysisErr.Type != ErrTypeMismatch {
			t.Errorf("%s: error = %v, want a type mismatch", sql, err)
		}
	}
}
