package storage

import (
	"fmt"
	"sync"
	"testing"
)

func TestIndexedReadWhileSharedGateCommitsInvalidate(t *testing.T) {
	_, _, schemas, tables := newTestSession(t)
	createTestTable(t, schemas, "t", []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "v", Type: "INTEGER"}})
	if _, err := tables.InsertBulk("t", []Row{{"id": int64(1), "v": int64(1)}, {"id": int64(2), "v": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := schemas.CreateIndex(&Index{Name: "idx", Table: "t", Columns: []IndexColumn{{Name: "v"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tables.BuildIndex("idx", "t", []string{"v"}); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		session := NewSession(schemas, tables)
		for i := 0; i < 500; i++ {
			if err := session.Begin(); err != nil {
				errors <- err
				return
			}
			if _, _, err := session.UpdateByPK("t", "2", func(Row) (Row, error) { return Row{"v": int64(2)}, nil }); err != nil {
				errors <- err
				return
			}
			if err := session.Commit(); err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 500; i++ {
			rows, err := tables.SelectByIndex("t", "idx", int64(1))
			if err != nil {
				errors <- err
				return
			}
			if len(rows) != 1 || rows[0]["id"] != int64(1) {
				errors <- fmt.Errorf("unchanged indexed row disappeared: %v", rows)
				return
			}
		}
	}()
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}
