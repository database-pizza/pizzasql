package storage

import (
	"errors"
	"strings"
	"testing"
)

// The DDL commit publishes one storage transaction, so the rewrite budget must
// reject an oversized table before the batch is encoded. The boundary is
// inclusive: exactly the store's operation ceiling is still sendable.
func TestDDLBatchBudgetRejectsOversizedRewrite(t *testing.T) {
	budget := &ddlBatchBudget{}
	if err := budget.reserve("t", maxOperations, 0); err != nil {
		t.Fatalf("reserve at the operation ceiling = %v, want nil", err)
	}
	err := budget.reserve("t", 1, 0)
	var limit *ddlBatchLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("reserve past the operation ceiling = %v, want *ddlBatchLimitError", err)
	}
	if !strings.Contains(limit.Error(), "storage batch limit") {
		t.Fatalf("limit error message = %q, want the storage batch limit named", limit.Error())
	}
	if limit.Table != "t" {
		t.Fatalf("limit error table = %q, want t", limit.Table)
	}

	bytes := &ddlBatchBudget{}
	if err := bytes.reserve("t", 1, maxTransactionSize); err != nil {
		t.Fatalf("reserve at the byte ceiling = %v, want nil", err)
	}
	if err := bytes.reserve("t", 1, 1); !errors.As(err, &limit) {
		t.Fatalf("reserve past the byte ceiling = %v, want *ddlBatchLimitError", err)
	}
}
