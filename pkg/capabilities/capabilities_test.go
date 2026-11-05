package capabilities

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

func TestCurrentDocumentIsVersionedAndMarshalable(t *testing.T) {
	doc := Current()
	if doc.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if doc.Engine != "pizzasql" || doc.EngineVersion != version.String() {
		t.Fatalf("engine identity = %q/%q", doc.Engine, doc.EngineVersion)
	}

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round Document
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Constraints.Unique.SQLState != "23505" {
		t.Fatalf("unique SQLSTATE = %q, want 23505", round.Constraints.Unique.SQLState)
	}
	if round.Transactions.SerializationCode != "40001" {
		t.Fatalf("serialization SQLSTATE = %q, want 40001", round.Transactions.SerializationCode)
	}
	if round.ErrorCodes["0A000"] != "feature_not_supported" {
		t.Fatalf("0A000 mapping = %q", round.ErrorCodes["0A000"])
	}
	if !round.Notifications.Listen.Supported || !round.Notifications.Notify.Supported {
		t.Fatal("LISTEN/NOTIFY should be advertised as supported")
	}
	if round.Notifications.PayloadLimit != 8000 || round.Notifications.DurableReplay {
		t.Fatalf("unexpected notification limits: %+v", round.Notifications)
	}
	if !round.DDL.RenameColumnIndexes.Supported {
		t.Fatal("RENAME COLUMN must advertise index metadata rewriting")
	}
}

func TestJSONArrowOperatorsAreAdvertised(t *testing.T) {
	arrow := Current().JSON.ArrowOperators
	if !arrow.Supported {
		t.Fatal("-> and ->> should be advertised as supported")
	}
	if arrow.SQLState != "" {
		t.Fatalf("arrow operator SQLSTATE = %q, want none", arrow.SQLState)
	}
	if arrow.Status == "rejected" {
		t.Fatalf("arrow operator status = %q, want a supported status", arrow.Status)
	}
}

func TestTransactionalDDLIsAdvertised(t *testing.T) {
	ddl := Current().DDL
	if !ddl.Transactional.Supported {
		t.Fatalf("transactional DDL = %+v, want supported", ddl.Transactional)
	}
	if ddl.Transactional.Status != "staged_until_commit" {
		t.Fatalf("transactional DDL status = %q, want staged_until_commit", ddl.Transactional.Status)
	}
}

func TestAddColumnBackfillIsAdvertised(t *testing.T) {
	doc := Current()
	if !doc.DDL.AddColumnBackfill.Supported {
		t.Fatalf("add_column_backfill = %+v, want supported", doc.DDL.AddColumnBackfill)
	}
	if doc.DDL.AddColumnBackfill.Status != "rewrites_rows" {
		t.Fatalf("add_column_backfill status = %q, want rewrites_rows", doc.DDL.AddColumnBackfill.Status)
	}
	if doc.DDL.AddColumn.Status == "metadata_only" {
		t.Fatalf("add_column status = %q, but a DEFAULT rewrites rows", doc.DDL.AddColumn.Status)
	}
}

// The capability document must describe the current default semantics: constant
// and whitelisted clock defaults are supported, the clock ones are evaluated on
// write, and only the remaining non-constant forms are rejected.
func TestNonConstantDefaultsAreAdvertisedAsEvaluatedAtWriteTime(t *testing.T) {
	defaults := Current().Constraints.DefaultValues
	if !defaults.Supported {
		t.Fatalf("default_values = %+v, want supported", defaults)
	}
	if defaults.Status != "evaluated_at_write_time" {
		t.Fatalf("default_values status = %q, want evaluated_at_write_time", defaults.Status)
	}
	if defaults.SQLState != "0A000" {
		t.Fatalf("default_values SQLSTATE = %q, want 0A000 for the remaining rejected forms", defaults.SQLState)
	}
}

func TestCheckConstraintIsAdvertised(t *testing.T) {
	doc := Current()
	check := doc.Constraints.Check
	if !check.Supported {
		t.Fatalf("check = %+v, want supported", check)
	}
	if check.Status != "enforced" {
		t.Fatalf("check status = %q, want enforced", check.Status)
	}
	if check.SQLState != pgerror.CodeCheckViolation {
		t.Fatalf("check SQLSTATE = %q, want %q", check.SQLState, pgerror.CodeCheckViolation)
	}
	if check.Notes == "" {
		t.Fatal("check notes must describe the enforcement surface")
	}
	if got := doc.ErrorCodes[pgerror.CodeCheckViolation]; got != "check_violation" {
		t.Fatalf("23514 mapping = %q, want check_violation", got)
	}
}

func TestDDLNotesDescribeCheckDependencies(t *testing.T) {
	doc := Current()
	if !strings.Contains(doc.DDL.DropColumn.Notes, "CHECK") || !strings.Contains(doc.DDL.DropColumn.Notes, "0A000") {
		t.Fatalf("drop_column notes = %q, want the persisted CHECK restriction", doc.DDL.DropColumn.Notes)
	}
	if !strings.Contains(doc.DDL.RenameColumn.Notes, "CHECK") || !strings.Contains(doc.DDL.RenameColumn.Notes, "0A000") {
		t.Fatalf("rename_column notes = %q, want the persisted CHECK restriction", doc.DDL.RenameColumn.Notes)
	}
	if !strings.Contains(doc.DDL.AddColumnBackfill.Notes, "23514") {
		t.Fatalf("add_column_backfill notes = %q, want the CHECK backfill failure", doc.DDL.AddColumnBackfill.Notes)
	}
}

// Composite primary keys are advertised as supported, with the SQLSTATE callers
// should expect for a duplicate key.
func TestCompositePrimaryKeyIsAdvertised(t *testing.T) {
	doc := Current()
	composite := doc.Constraints.CompositePK
	if !composite.Supported {
		t.Fatalf("composite_primary_key = %+v, want supported", composite)
	}
	if composite.Status == "rejected" {
		t.Fatalf("composite_primary_key status = %q, want a supported status", composite.Status)
	}
	if composite.SQLState != "23505" {
		t.Fatalf("composite_primary_key SQLSTATE = %q, want 23505", composite.SQLState)
	}
	if !doc.Constraints.PrimaryKey.Supported {
		t.Fatalf("primary_key = %+v, want supported", doc.Constraints.PrimaryKey)
	}

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"composite_primary_key":{"supported":true`) {
		t.Fatalf("document does not advertise composite_primary_key as supported: %s", body)
	}
}

// Foreign keys are advertised as enforced, with the SQLSTATE callers should
// expect for a dangling reference, the referential actions that execute
// immediately, and the explicit exclusions for deferred enforcement.
func TestForeignKeyIsAdvertised(t *testing.T) {
	doc := Current()
	fk := doc.Constraints.ForeignKey
	if !fk.Supported {
		t.Fatalf("foreign_key = %+v, want supported", fk)
	}
	if fk.Status != "enforced" {
		t.Fatalf("foreign_key status = %q, want enforced", fk.Status)
	}
	if fk.SQLState != pgerror.CodeForeignKeyViolation {
		t.Fatalf("foreign_key SQLSTATE = %q, want %q", fk.SQLState, pgerror.CodeForeignKeyViolation)
	}
	for _, want := range []string{
		"UNIQUE", "COMMIT", "MATCH SIMPLE", "MATCH keyword", "self",
		"CASCADE", "SET NULL", "SET DEFAULT", "NOT NULL", "transitively",
		"autocommit", "no later than COMMIT", "statement savepoint", "hand-authored catalog",
		"O(FK component)", "DEFERRABLE", "0A000",
		"PRAGMA foreign_keys", "rejects OFF", "foreign_key_list", "database-wide foreign-key write gate",
		"DROP/RENAME COLUMN", "DROP TABLE", "pg_constraint",
	} {
		if !strings.Contains(fk.Notes, want) {
			t.Fatalf("foreign_key notes = %q, want it to mention %q", fk.Notes, want)
		}
	}
	if got := doc.ErrorCodes[pgerror.CodeForeignKeyViolation]; got != "foreign_key_violation" {
		t.Fatalf("23503 mapping = %q, want foreign_key_violation", got)
	}
}

func TestArraysAreAdvertised(t *testing.T) {
	doc := Current()
	types := doc.Types.Arrays
	if !types.Supported {
		t.Fatalf("types.arrays = %+v, want supported", types)
	}
	if types.Status != "one_dimensional" {
		t.Fatalf("types.arrays status = %q, want one_dimensional", types.Status)
	}
	for _, want := range []string{"TEXT[]", "INTEGER[]", "BOOLEAN[]", "ARRAY[", "array_length", "unnest", "0A000", "22P02"} {
		if !strings.Contains(types.Notes, want) {
			t.Errorf("types.arrays notes do not mention %q", want)
		}
	}

	params := doc.Parameters.Arrays
	if !params.Supported {
		t.Fatalf("parameters.arrays = %+v, want supported", params)
	}
	if params.Status != "text_format" {
		t.Fatalf("parameters.arrays status = %q, want text_format", params.Status)
	}
	for _, want := range []string{"1009", "1007", "1016", "1000", "1022", "1021", "1231", "1015", "0A000"} {
		if !strings.Contains(params.Notes, want) {
			t.Errorf("parameters.arrays notes do not mention %q", want)
		}
	}
	if !strings.Contains(doc.Parameters.MaxOID, "text[]/varchar[]/int4[]") {
		t.Fatalf("parameters.max_oid = %q, want the array OIDs listed", doc.Parameters.MaxOID)
	}
}

// JSONB is advertised as a native type with precise canonicalization, operator,
// function, ordering, parameter, and rejection claims.
func TestJSONBIsAdvertised(t *testing.T) {
	doc := Current()
	types := doc.Types.JSONB
	if !types.Supported {
		t.Fatalf("types.jsonb = %+v, want supported", types)
	}
	if types.Status != "native" {
		t.Fatalf("types.jsonb status = %q, want native", types.Status)
	}
	for _, want := range []string{"canonical", "sorted", "last-wins", "idempotent", "OID 3802", "22P02", "0A000", "jsonpath", "GIN"} {
		if !strings.Contains(types.Notes, want) {
			t.Errorf("types.jsonb notes do not mention %q", want)
		}
	}
	if !doc.Types.JSONText {
		t.Fatal("JSON (text) remains supported and must stay advertised")
	}

	jsonb := doc.JSON.JSONB
	for _, want := range []string{"sorted", "last-wins", "1.50", "idempotent"} {
		if !strings.Contains(jsonb.Canonicalization, want) {
			t.Errorf("json.jsonb.canonicalization does not mention %q", want)
		}
	}
	for _, want := range []string{"exact canonical text bytes", "DISTINCT", "ORDER BY", "numeric"} {
		if !strings.Contains(jsonb.Ordering, want) {
			t.Errorf("json.jsonb.ordering does not mention %q", want)
		}
	}
	if !jsonb.Operators.Supported || jsonb.Operators.SQLState != pgerror.CodeDatatypeMismatch {
		t.Fatalf("json.jsonb.operators = %+v", jsonb.Operators)
	}
	for _, want := range []string{"@>", "<@", "?|", "?&", "#>", "#>>", "#-", "||", "SQL NULL"} {
		if !strings.Contains(jsonb.Operators.Notes, want) {
			t.Errorf("json.jsonb.operators notes do not mention %q", want)
		}
	}
	if !jsonb.Functions.Supported || jsonb.Functions.SQLState != pgerror.CodeInvalidParameter {
		t.Fatalf("json.jsonb.functions = %+v", jsonb.Functions)
	}
	for _, want := range []string{"jsonb_typeof", "jsonb_array_length", "jsonb_strip_nulls", "jsonb_pretty", "jsonb_set", "jsonb_insert", "jsonb_build_object", "jsonb_build_array", "to_jsonb"} {
		if !strings.Contains(jsonb.Functions.Notes, want) {
			t.Errorf("json.jsonb.functions notes do not mention %q", want)
		}
	}
	if !jsonb.SetReturning.Supported || jsonb.SetReturning.Status != "single_from_item" {
		t.Fatalf("json.jsonb.set_returning = %+v", jsonb.SetReturning)
	}
	for _, want := range []string{"jsonb_each", "jsonb_array_elements", "jsonb_object_keys", "0A000"} {
		if !strings.Contains(jsonb.SetReturning.Notes, want) {
			t.Errorf("json.jsonb.set_returning notes do not mention %q", want)
		}
	}
	if !jsonb.Parameters.Supported || jsonb.Parameters.Status != "text_and_binary" {
		t.Fatalf("json.jsonb.parameters = %+v", jsonb.Parameters)
	}
	for _, want := range []string{"3802", "0x01", "pgx", "22P02"} {
		if !strings.Contains(jsonb.Parameters.Notes, want) {
			t.Fatalf("json.jsonb.parameters notes do not mention %q: %q", want, jsonb.Parameters.Notes)
		}
	}
	if jsonb.Rejected.Supported || jsonb.Rejected.SQLState != pgerror.CodeFeatureNotSupported {
		t.Fatalf("json.jsonb.rejected = %+v", jsonb.Rejected)
	}
	for _, want := range []string{"jsonpath", "GIN", "expression indexes", "wildcard"} {
		if !strings.Contains(jsonb.Rejected.Notes, want) {
			t.Errorf("json.jsonb.rejected notes do not mention %q", want)
		}
	}
	if !strings.Contains(doc.Parameters.MaxOID, "3802") {
		t.Fatalf("parameters.max_oid = %q, want OID 3802 listed", doc.Parameters.MaxOID)
	}
	if got := doc.ErrorCodes[pgerror.CodeInvalidParameter]; got != "invalid_parameter_value" {
		t.Fatalf("22023 mapping = %q, want invalid_parameter_value", got)
	}
}
