package executor

import (
	"testing"
)

// TestGoatCounterSQLiteMigrations runs the shapes of GoatCounter's SQLite
// migrations that exercise the engine features added for the release-2.7 port:
// JSON1 mutation/extraction, bitwise OR, and table-level UNIQUE ... ON CONFLICT
// REPLACE.
func TestGoatCounterSQLiteMigrations(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, `CREATE TABLE sites (
		site_id INTEGER PRIMARY KEY AUTOINCREMENT,
		settings TEXT NOT NULL DEFAULT '{}',
		user_defaults TEXT NOT NULL DEFAULT '{}'
	)`)
	execMust(t, e, `CREATE TABLE users (
		user_id INTEGER PRIMARY KEY AUTOINCREMENT,
		settings TEXT NOT NULL DEFAULT '{}'
	)`)
	execMust(t, e, `INSERT INTO sites (site_id, settings, user_defaults) VALUES
		(1, '{"public": 1, "collect": 0, "widgets": []}', '{"widgets": []}')`)
	execMust(t, e, `INSERT INTO users (user_id, settings) VALUES (1, '{"widgets": []}')`)

	// db/migrate/2021-06-27-1-public-sqlite.sql
	execMust(t, e, `UPDATE sites SET settings = json_set(settings, '$.public', 'public') WHERE json_extract(settings, '$.public') = 1`)
	execMust(t, e, `UPDATE sites SET settings = json_set(settings, '$.public', 'private') WHERE json_extract(settings, '$.public') = 0`)

	res := execMust(t, e, `SELECT json_extract(settings, '$.public') FROM sites WHERE site_id = 1`)
	if res.Rows[0][0] != "public" {
		t.Fatalf("public flag = %v, want public", res.Rows[0][0])
	}

	// db/migrate/2021-12-02-2-language-enable-sqlite.sql
	execMust(t, e, `UPDATE sites SET
		settings = json_replace(settings, '$.collect', json_extract(settings, '$.collect') | 64),
		user_defaults = json_replace(user_defaults, '$.widgets', json_insert(json_extract(user_defaults, '$.widgets'), '$[#]', json('{"n":"languages"}')))`)
	execMust(t, e, `UPDATE users SET
		settings = json_replace(settings, '$.widgets', json_insert(json_extract(settings, '$.widgets'), '$[#]', json('{"n":"languages"}')))`)

	res = execMust(t, e, `SELECT json_extract(settings, '$.collect') FROM sites WHERE site_id = 1`)
	if res.Rows[0][0] != int64(64) {
		t.Fatalf("collect flag = %v, want 64", res.Rows[0][0])
	}
	res = execMust(t, e, `SELECT json_extract(user_defaults, '$.widgets[0].n') FROM sites WHERE site_id = 1`)
	if res.Rows[0][0] != "languages" {
		t.Fatalf("widgets[0].n = %v, want languages", res.Rows[0][0])
	}

	// A GoatCounter stats table as created by 2022-01-13-1-unfk-sqlite.sql:
	// composite UNIQUE with ON CONFLICT REPLACE and no primary key.
	execMust(t, e, `CREATE TABLE hit_counts (
		site_id INTEGER NOT NULL,
		path_id INTEGER NOT NULL,
		hour TEXT NOT NULL,
		total INTEGER NOT NULL,
		CONSTRAINT "hit_counts#site_id#path_id#hour" UNIQUE(site_id, path_id, hour) ON CONFLICT REPLACE
	)`)
	execMust(t, e, `INSERT INTO hit_counts (site_id, path_id, hour, total) VALUES (1, 1, '2024-01-01 00:00:00', 5)`)
	execMust(t, e, `INSERT INTO hit_counts (site_id, path_id, hour, total) VALUES (1, 1, '2024-01-01 00:00:00', 9)`)
	res = execMust(t, e, `SELECT total FROM hit_counts WHERE site_id = 1 AND path_id = 1`)
	if res.RowCount != 1 || res.Rows[0][0] != int64(9) {
		t.Fatalf("hit_counts replace = %v rows %v", res.Rows, res.Rows)
	}
}

// TestGoatCounterMigrationRebuildShape covers the table-rebuild dance used by
// 2021-12-09-1-email-reports-sqlite.sql and 2022-01-13-1-unfk-sqlite.sql:
// create a replacement table, copy rows with INSERT ... SELECT, drop and rename,
// then build indexes (including composite expression unique indexes).
func TestGoatCounterMigrationRebuildShape(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)

	execMust(t, e, "CREATE TABLE users (user_id INTEGER PRIMARY KEY AUTOINCREMENT, site_id INTEGER, email TEXT, seen_updates_at TIMESTAMP)")
	execMust(t, e, "INSERT INTO users (site_id, email) VALUES (1, 'A@x.com'), (1, 'b@x.com'), (2, 'a@x.com')")

	execMust(t, e, `CREATE TABLE users2 (
		user_id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL,
		email VARCHAR NOT NULL,
		last_report_at TIMESTAMP NOT NULL DEFAULT current_timestamp
	)`)
	execMust(t, e, `INSERT INTO users2 (user_id, site_id, email)
		SELECT user_id, site_id, email FROM users`)
	execMust(t, e, "DROP TABLE users")
	execMust(t, e, "ALTER TABLE users2 RENAME TO users")
	execMust(t, e, `CREATE INDEX "users#site_id" ON users(site_id)`)
	execMust(t, e, `CREATE UNIQUE INDEX "users#site_id#email" ON users(site_id, lower(email))`)

	res := execMust(t, e, "SELECT count(*) FROM users")
	if res.Rows[0][0] != int64(3) {
		t.Fatalf("copied row count = %v, want 3", res.Rows[0][0])
	}
	// Case-insensitive uniqueness across the composite expression index.
	if _, err := execSQL(e, "INSERT INTO users (site_id, email) VALUES (1, 'a@x.com')"); err == nil {
		t.Fatal("expected composite expression unique index to reject a duplicate")
	}
}

func TestGoatCounterInsertWithSelectMigrationShape(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE old_sizes (size TEXT)")
	execMust(t, e, "CREATE TABLE sizes (width INTEGER, height INTEGER)")
	execMust(t, e, "INSERT INTO old_sizes VALUES ('10,20'), ('30,40')")

	execMust(t, e, `INSERT INTO sizes (width, height)
		WITH source AS (
			SELECT size FROM old_sizes GROUP BY size
		)
		SELECT
			CAST(substr(size, 1, instr(size, ',') - 1) AS INTEGER),
			CAST(substr(size, instr(size, ',') + 1) AS INTEGER)
		FROM source`)
	res := execMust(t, e, "SELECT width, height FROM sizes ORDER BY width")
	if res.RowCount != 2 || res.Rows[0][0] != int64(10) || res.Rows[1][1] != int64(40) {
		t.Fatalf("migrated sizes = %v", res.Rows)
	}
}

// TestGoatCounterDropSizesShape covers 2025-06-21-2-drop-sizes.sql, which adds a
// column, backfills it with a correlated scalar subquery, drops a column, and
// drops the source table.
func TestGoatCounterDropSizesShape(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE sizes (size_id INTEGER PRIMARY KEY, width INTEGER)")
	execMust(t, e, "CREATE TABLE hits (id INTEGER PRIMARY KEY, size_id INTEGER)")
	execMust(t, e, "INSERT INTO sizes VALUES (1, 480), (2, 720)")
	execMust(t, e, "INSERT INTO hits (id, size_id) VALUES (1, 1), (2, 2)")

	execMust(t, e, "ALTER TABLE hits ADD COLUMN width SMALLINT NULL")
	execMust(t, e, "UPDATE hits SET width = (SELECT width FROM sizes WHERE size_id = hits.size_id)")
	execMust(t, e, "ALTER TABLE hits DROP COLUMN size_id")
	execMust(t, e, "DROP TABLE sizes")

	res := execMust(t, e, "SELECT id, width FROM hits ORDER BY id")
	if res.Rows[0][1] != int64(480) || res.Rows[1][1] != int64(720) {
		t.Fatalf("backfilled widths = %v", res.Rows)
	}
	if _, err := execSQL(e, "SELECT size_id FROM hits"); err == nil {
		t.Fatal("size_id should have been dropped")
	}
}

func TestGoatCounterDropThenRenameColumnMigration(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE hit_counts (id INTEGER PRIMARY KEY, total INTEGER, total_unique INTEGER)")
	execMust(t, e, "INSERT INTO hit_counts VALUES (1, 111, 222)")

	execMust(t, e, "ALTER TABLE hit_counts DROP COLUMN total")
	execMust(t, e, "ALTER TABLE hit_counts RENAME COLUMN total_unique TO total")
	res := execMust(t, e, "SELECT total FROM hit_counts WHERE id = 1")
	if res.Rows[0][0] != int64(222) {
		t.Fatalf("renamed total = %v, want 222", res.Rows[0][0])
	}
}
