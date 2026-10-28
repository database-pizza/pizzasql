package capabilities

import (
	"encoding/json"
	"testing"

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
}
