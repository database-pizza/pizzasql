package capabilities

import (
	"encoding/json"
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

func TestAddColumnBackfillIsAdvertised(t *testing.T) {
	doc := Current()
	if !doc.DDL.AddColumnBackfill.Supported {
		t.Fatalf("add_column_backfill = %+v, want supported", doc.DDL.AddColumnBackfill)
	}
	if doc.DDL.AddColumnBackfill.Status != "rewrites_rows" {
		t.Fatalf("add_column_backfill status = %q, want rewrites_rows", doc.DDL.AddColumnBackfill.Status)
	}
	if doc.DDL.AddColumn.Status == "metadata_only" {
		t.Fatalf("add_column status = %q, but a constant DEFAULT rewrites rows", doc.DDL.AddColumn.Status)
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
