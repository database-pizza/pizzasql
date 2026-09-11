package analyzer

import "testing"

func analyze(t *testing.T, a *Analyzer, sql string) error {
	t.Helper()
	return a.Analyze(parse(t, sql))
}

func TestAnalyzeBitwiseOperators(t *testing.T) {
	a := New(setupCatalog())
	for _, sql := range []string{
		"SELECT age & 3 FROM users",
		"SELECT age | 3 FROM users",
		"SELECT age << 1 FROM users",
		"SELECT age >> 1 FROM users",
		"SELECT ~age FROM users",
	} {
		if err := analyze(t, a, sql); err != nil {
			t.Errorf("%s: unexpected error: %v", sql, err)
		}
	}
}

func TestAnalyzeJSONAndPercentDiffFunctions(t *testing.T) {
	a := New(setupCatalog())
	for _, sql := range []string{
		`SELECT json('[1,2]')`,
		`SELECT json_extract('{"a":1}', '$.a')`,
		`SELECT json_set('{}', '$.a', 1)`,
		`SELECT json_insert('[]', '$[#]', json('1'))`,
		`SELECT json_replace('{"a":1}', '$.a', 2)`,
		`SELECT json_group_array(id) FROM users`,
		`SELECT percent_diff(1, 2)`,
	} {
		if err := analyze(t, a, sql); err != nil {
			t.Errorf("%s: unexpected error: %v", sql, err)
		}
	}
}

func TestAnalyzeReturning(t *testing.T) {
	a := New(setupCatalog())
	for _, sql := range []string{
		"INSERT INTO users (name) VALUES ('x') RETURNING id, name",
		"UPDATE users SET name = 'y' RETURNING id",
		"DELETE FROM users WHERE id = 1 RETURNING *",
	} {
		if err := analyze(t, a, sql); err != nil {
			t.Errorf("%s: unexpected error: %v", sql, err)
		}
	}
}

func TestAnalyzeIsDistinctFromAndAnalyze(t *testing.T) {
	a := New(setupCatalog())
	for _, sql := range []string{
		"SELECT age IS DISTINCT FROM 1 FROM users",
		"SELECT age IS NOT DISTINCT FROM 1 FROM users",
		"ANALYZE",
		"ANALYZE users",
	} {
		if err := analyze(t, a, sql); err != nil {
			t.Errorf("%s: unexpected error: %v", sql, err)
		}
	}
}

func TestAnalyzeUpdateFrom(t *testing.T) {
	a := New(setupCatalog())
	// orders.user_id joins users.id; the derived table exposes a count column.
	err := analyze(t, a, `WITH x AS (SELECT count(*) AS n, id FROM users GROUP BY id)
		UPDATE orders SET status = 'y' FROM x WHERE x.id = orders.user_id`)
	if err != nil {
		t.Fatalf("UPDATE ... FROM should analyze: %v", err)
	}
}

func TestAnalyzeGeneratedColumns(t *testing.T) {
	catalog := setupCatalog()
	catalog.CreateTable(&TableInfo{
		Name: "metrics",
		Columns: []ColumnInfo{
			{Name: "a", Type: TypeInteger, Nullable: true},
			{Name: "b", Type: TypeInteger, Nullable: true, Generated: true},
		},
	})
	a := New(catalog)

	if err := analyze(t, a, "INSERT INTO metrics (a, b) VALUES (1, 2)"); err == nil {
		t.Fatal("expected generated-column insert to be rejected")
	}
	if err := analyze(t, a, "UPDATE metrics SET b = 2"); err == nil {
		t.Fatal("expected generated-column update to be rejected")
	}
	if err := analyze(t, a, "INSERT INTO metrics (a) VALUES (1)"); err != nil {
		t.Fatalf("plain insert should analyze: %v", err)
	}
}
