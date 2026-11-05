package storage

import "testing"

// A RENAME that returns to the committed name must not leave a pending move:
// publishing a put and a delete for the same key in one batch is order-dependent
// and can lose the row or the schema record. Assert the property directly on the
// transaction's catalog and row ops.
func TestSchemaTxRenameAwayAndBackHasNoDuplicateOps(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "a", []Column{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "v", Type: "TEXT"},
	})
	if err := tables.Insert("a", Row{"id": int64(1), "v": "one"}); err != nil {
		t.Fatal(err)
	}

	s := NewSession(schemas, tables)
	if err := s.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameTable("a", "b"); err != nil {
		t.Fatalf("rename a->b: %v", err)
	}
	if err := s.RenameTable("b", "a"); err != nil {
		t.Fatalf("rename b->a: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Renaming back to the committed name cancels the pending move, so the
	// overlay has nothing left to publish for it.
	if len(s.schemaTx.renames) != 0 {
		t.Fatalf("pending renames after renaming away and back = %v, want none", s.schemaTx.renames)
	}
	if source := s.schemaTx.dataSources["a"]; source != "a" {
		t.Fatalf("data source for a = %q, want a", source)
	}
	catalogOps, _, err := s.schemaTx.commitCatalogOps()
	if err != nil {
		t.Fatalf("catalog ops: %v", err)
	}
	dataOps, _, err := s.schemaTx.commitDataOps(&ddlBatchBudget{})
	if err != nil {
		t.Fatalf("data ops: %v", err)
	}
	seen := make(map[string]bool, len(catalogOps)+len(dataOps))
	for _, op := range append(catalogOps, dataOps...) {
		key := string(op.Key)
		if seen[key] {
			t.Fatalf("rename away and back emitted more than one operation for key %q", key)
		}
		seen[key] = true
	}
}
