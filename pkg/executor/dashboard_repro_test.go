package executor

import "testing"

// TestSelectColumnTypesGroupBy verifies that GROUP BY projections keep their
// schema types, so protocol clients can decode timestamp/date columns even
// though the grouped projection is not a plain table scan.
func TestSelectColumnTypesGroupBy(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE hit_counts (
		site_id INTEGER NOT NULL,
		path_id INTEGER NOT NULL,
		hour TIMESTAMP NOT NULL,
		total INTEGER NOT NULL
	)`)
	execMust(t, e, `CREATE TABLE hit_stats (
		site_id INTEGER NOT NULL,
		path_id INTEGER NOT NULL,
		day DATE NOT NULL,
		stats TEXT
	)`)
	execMust(t, e, `INSERT INTO hit_counts VALUES (1, 1, '2024-05-06 07:00:00', 3)`)
	execMust(t, e, `INSERT INTO hit_stats VALUES (1, 1, '2024-05-06', '[]')`)

	res := execMust(t, e, `SELECT hour, sum(total) FROM hit_counts GROUP BY hour ORDER BY hour`)
	want := []string{"TIMESTAMP", "TEXT"}
	if len(res.ColumnTypes) != len(want) {
		t.Fatalf("group-by column types = %v, want %v", res.ColumnTypes, want)
	}
	for i := range want {
		if res.ColumnTypes[i] != want[i] {
			t.Fatalf("group-by column types = %v, want %v", res.ColumnTypes, want)
		}
	}

	res = execMust(t, e, `SELECT path_id, day, stats FROM hit_stats ORDER BY day`)
	want = []string{"INTEGER", "DATE", "TEXT"}
	if len(res.ColumnTypes) != len(want) {
		t.Fatalf("date column types = %v, want %v", res.ColumnTypes, want)
	}
	for i := range want {
		if res.ColumnTypes[i] != want[i] {
			t.Fatalf("date column types = %v, want %v", res.ColumnTypes, want)
		}
	}
}

// TestSubstrSQLiteSemantics pins SQLite's zero/negative start behavior, which
// GoatCounter relies on to derive a country code from a region code.
func TestSubstrSQLiteSemantics(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	cases := []struct {
		sql  string
		want string
	}{
		{`SELECT substr('US-NY', 0, 3)`, "US"},
		{`SELECT substr('US-NY', 1, 3)`, "US-"},
		{`SELECT substr('US-NY', 2, 3)`, "S-N"},
		{`SELECT substr('US-NY', -2, 2)`, "NY"},
		{`SELECT substr('US-NY', 4)`, "NY"},
		{`SELECT substr('US-NY', 0)`, "US-NY"},
	}
	for _, tc := range cases {
		res := execMust(t, e, tc.sql)
		if len(res.Rows) != 1 || res.Rows[0][0] != tc.want {
			t.Fatalf("%s = %#v, want %q", tc.sql, res.Rows, tc.want)
		}
	}
}

// TestCTEGroupByAlias reproduces the GoatCounter locations query: a CTE that
// groups by a SELECT alias, with the outer query joining on that alias column.
func TestCTEGroupByAlias(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, `CREATE TABLE location_stats (
		site_id INTEGER NOT NULL,
		path_id INTEGER NOT NULL,
		day DATE NOT NULL,
		location TEXT NOT NULL,
		count INTEGER NOT NULL
	)`)
	execMust(t, e, `CREATE TABLE locations (
		iso_3166_2 TEXT NOT NULL,
		country_name TEXT NOT NULL
	)`)
	execMust(t, e, `INSERT INTO location_stats VALUES (1, 1, '2024-05-06', 'US-NY', 4)`)
	execMust(t, e, `INSERT INTO locations VALUES ('US', 'United States')`)

	res := execMust(t, e, `WITH x AS (
		SELECT substr(location, 0, 3) AS loc, sum(count) AS count
		FROM location_stats
		WHERE site_id = 1 AND day >= '2024-01-01' AND day <= '2024-12-31'
		GROUP BY loc
		ORDER BY count DESC, loc
		LIMIT 5
	)
	SELECT locations.iso_3166_2 AS id, locations.country_name AS name, x.count AS count
	FROM x
	JOIN locations ON locations.iso_3166_2 = x.loc
	ORDER BY count DESC, name ASC`)

	if res.RowCount != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", res.RowCount, res.Rows)
	}
	if res.Rows[0][0] != "US" || res.Rows[0][2].(int64) != 4 {
		t.Fatalf("unexpected row %#v", res.Rows[0])
	}
}
