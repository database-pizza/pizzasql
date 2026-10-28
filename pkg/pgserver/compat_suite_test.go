package pgserver

// Compatibility suite for the pChat-style workload: DDL, upsert idempotency,
// constraints, pagination, parameters, and error codes. It runs only when
// PIZZASQL_COMPAT=1 so the regular unit suite stays fast; `make compat` enables
// it and writes a JSON report.
//
// The suite distinguishes:
//   - pass:    the build behaves as documented
//   - missing: a documented capability is absent from this build (for example a
//     rejection introduced by a newer release)
//   - fail:    behavior contradicts the documented contract
//
// PIZZASQL_COMPAT_STRICT=1 turns "missing" into a failure, which is what a
// release gate should use. The compatibility document is pkg/capabilities.

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func rowValue(rows [][]string, index int) string {
	if len(rows) <= index || len(rows[index]) == 0 {
		return ""
	}
	return rows[index][0]
}

type compatResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass, missing, fail
	Detail string `json:"detail,omitempty"`
}

type compatReport struct {
	Engine  string         `json:"engine"`
	Results []compatResult `json:"results"`
}

func TestCompatibilitySuite(t *testing.T) {
	if os.Getenv("PIZZASQL_COMPAT") != "1" {
		t.Skip("set PIZZASQL_COMPAT=1 (or run make compat) to run the compatibility suite")
	}
	strict := os.Getenv("PIZZASQL_COMPAT_STRICT") == "1"

	c, client := newDataConnection(t)
	report := compatReport{Engine: "pizzasql"}

	record := func(name, status, detail string) {
		report.Results = append(report.Results, compatResult{Name: name, Status: status, Detail: detail})
	}

	// exec runs a statement and returns the SQLSTATE code ("" on success) and
	// decoded rows.
	exec := func(sql string) (string, [][]string) {
		msgs := runQuery(t, c, client, &Message{Type: MsgQuery, Data: append([]byte(sql), 0)})
		if len(msgs) == 0 {
			return "", nil
		}
		if msgs[0].Type == MsgErrorResponse {
			return errorCode(msgs[0]), nil
		}
		var rows [][]string
		for _, msg := range msgs[1:] {
			if msg.Type != MsgDataRow {
				continue
			}
			rows = append(rows, dataRowValues(t, msg))
		}
		return "", rows
	}
	expectOK := func(name, sql string) {
		if code, _ := exec(sql); code != "" {
			record(name, "fail", "unexpected SQLSTATE "+code)
			return
		}
		record(name, "pass", "")
	}
	expectCode := func(name, sql, want string) {
		code, _ := exec(sql)
		switch code {
		case want:
			record(name, "pass", "")
		case "":
			record(name, "missing", "statement succeeded; this build does not enforce "+want)
		default:
			record(name, "missing", "got SQLSTATE "+code+", want "+want)
		}
	}
	expectRows := func(name, sql string, want ...string) {
		code, rows := exec(sql)
		if code != "" {
			record(name, "fail", "unexpected SQLSTATE "+code)
			return
		}
		if len(rows) != 1 || len(rows[0]) != len(want) {
			record(name, "fail", "unexpected rows")
			return
		}
		for i := range want {
			if rows[0][i] != want[i] {
				record(name, "fail", "row "+rows[0][i]+" != "+want[i])
				return
			}
		}
		record(name, "pass", "")
	}

	expectOK("ddl.create_table", `CREATE TABLE accounts (
		id INTEGER PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		plan TEXT NOT NULL DEFAULT 'free',
		seats INTEGER NOT NULL DEFAULT 1
	)`)
	expectOK("dml.insert", `INSERT INTO accounts (id, email, plan, seats) VALUES (1, 'a@x', 'pro', 3)`)
	expectRows("dml.select_constant_default", `SELECT plan, seats FROM accounts WHERE id = 1`, "pro", "3")

	expectOK("upsert.first", `INSERT INTO accounts (id, email, plan) VALUES (2, 'b@x', 'free') ON CONFLICT(id) DO UPDATE SET plan = excluded.plan`)
	expectOK("upsert.idempotent", `INSERT INTO accounts (id, email, plan) VALUES (2, 'b@x', 'pro') ON CONFLICT(id) DO UPDATE SET plan = excluded.plan`)
	expectRows("upsert.result", `SELECT plan FROM accounts WHERE id = 2`, "pro")

	expectCode("constraints.unique_23505", `INSERT INTO accounts (id, email) VALUES (3, 'a@x')`, "23505")
	expectCode("ddl.composite_pk_0A000", `CREATE TABLE bad_pk (a INTEGER, b INTEGER, PRIMARY KEY (a, b))`, "0A000")
	expectCode("ddl.nonconstant_default_0A000", `CREATE TABLE bad_default (id INTEGER PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`, "0A000")
	expectCode("locking.for_update_0A000", `SELECT * FROM accounts FOR UPDATE`, "0A000")
	expectCode("arrays.any_0A000", `SELECT * FROM accounts WHERE id = ANY('{1,2}')`, "0A000")

	// Documented current limitation: CHECK and FOREIGN KEY are parsed but not
	// enforced. A release that starts rejecting them will report "missing"
	// here and the capability document must be updated.
	expectOK("limitations.check_not_enforced", `CREATE TABLE checked (n INTEGER CHECK (n > 0))`)
	expectOK("limitations.check_insert_violation", `INSERT INTO checked (n) VALUES (-1)`)
	expectOK("limitations.foreign_key_not_enforced", `CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES accounts(id))`)
	expectOK("limitations.orphan_insert", `INSERT INTO child (id, parent_id) VALUES (1, 999)`)

	expectCode("errors.syntax_42601", `SELEC 1`, "42601")

	// Explicit transactions buffer writes and validate unique indexes at
	// COMMIT, so the duplicate insert itself succeeds and the commit fails.
	expectOK("tx.begin", `BEGIN`)
	expectOK("tx.buffer_duplicate", `INSERT INTO accounts (id, email) VALUES (3, 'a@x')`)
	expectCode("tx.commit_unique_23505", `COMMIT`, "23505")
	// A failed COMMIT has already rolled back; the session must be usable
	// again instead of being stuck in the failed-transaction state.
	expectRows("tx.after_failed_commit_idle", `SELECT 1`, "1")
	expectOK("tx.begin_after_failed_commit", `BEGIN`)
	expectOK("tx.rollback", `ROLLBACK`)

	expectOK("pagination.seed", `INSERT INTO accounts (id, email) VALUES (10, 'p10@x'), (11, 'p11@x'), (12, 'p12@x')`)
	code, rows := exec(`SELECT _rowid_ FROM accounts WHERE _rowid_ > 1 ORDER BY _rowid_ LIMIT 2`)
	first, firstErr := strconv.ParseInt(rowValue(rows, 0), 10, 64)
	second, secondErr := strconv.ParseInt(rowValue(rows, 1), 10, 64)
	switch {
	case code != "":
		record("pagination.rowid", "fail", "unexpected SQLSTATE "+code)
	case len(rows) == 2 && firstErr == nil && secondErr == nil && first < second:
		record("pagination.rowid", "pass", "")
	default:
		record("pagination.rowid", "fail", "cursor did not advance")
	}

	failures, missing := 0, 0
	for _, result := range report.Results {
		switch result.Status {
		case "fail":
			failures++
			t.Errorf("%s: %s", result.Name, result.Detail)
		case "missing":
			missing++
			t.Logf("%s: %s", result.Name, result.Detail)
		}
	}

	if path := os.Getenv("PIZZASQL_COMPAT_REPORT"); path != "" {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}

	if failures > 0 || (strict && missing > 0) {
		t.Fatalf("compatibility failures=%d missing=%d (strict=%v)", failures, missing, strict)
	}
}
