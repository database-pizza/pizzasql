package storage

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func startPizzaKVTest(t testing.TB, binary, socket, database string) func() {
	t.Helper()
	cmd := exec.Command(binary, "-unix="+socket, "-path="+database)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start PizzaKV: %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitPizzaKVPool(t testing.TB, socket string) *KVPool {
	t.Helper()
	addr := "unix:" + socket
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pool, err := NewKVPool(addr, 2, 5*time.Second)
		if err == nil {
			return pool
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PizzaKV did not become ready")
	return nil
}

func shortPizzaKVSocket(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pkv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestPizzaKVIntegration(t *testing.T) {
	binary := os.Getenv("PIZZAKV_BIN")
	if binary == "" {
		t.Skip("PIZZAKV_BIN is not set")
	}

	dir := t.TempDir()
	socket := shortPizzaKVSocket(t)
	database := filepath.Join(dir, "integration.pkvdb")
	stop := startPizzaKVTest(t, binary, socket, database)
	pool := waitPizzaKVPool(t, socket)

	schemas := NewSchemaManager(pool, "integration")
	tables := NewTableManager(pool, schemas, "integration")
	if err := schemas.CreateTable(&Schema{
		Name: "events",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "payload", Type: "BLOB", Nullable: true},
		},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for i := int64(1); i <= 20; i++ {
		payload := []byte{byte(i), 0, '|', '\r', '\n'}
		if err := tables.Insert("events", Row{"id": i, "payload": payload}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	largePayload := bytes.Repeat([]byte{0xab}, 2*1024*1024)
	if err := tables.Insert("events", Row{"id": int64(21), "payload": largePayload}); err != nil {
		t.Fatalf("insert large row: %v", err)
	}
	largeRows, err := tables.Select("events", func(row Row) bool { return row["id"] == int64(21) })
	if err != nil || len(largeRows) != 1 {
		t.Fatalf("scan large row: len=%d err=%v", len(largeRows), err)
	}
	if got, ok := largeRows[0]["payload"].([]byte); !ok || !bytes.Equal(got, largePayload) {
		t.Fatalf("large binary payload mismatch: len=%d type=%T", len(got), largeRows[0]["payload"])
	}
	rows, err := tables.SelectWithLimit("events", nil, 3, 2)
	if err != nil || len(rows) != 3 {
		t.Fatalf("limited select: len=%d err=%v", len(rows), err)
	}
	row, err := tables.GetByPK("events", "1")
	if err != nil {
		t.Fatalf("point read: %v", err)
	}
	if got, ok := row["payload"].([]byte); !ok || !bytes.Equal(got, []byte{1, 0, '|', '\r', '\n'}) {
		t.Fatalf("binary payload = %v (%T)", row["payload"], row["payload"])
	}
	if count, err := tables.CountFast("events"); err != nil || count != 21 {
		t.Fatalf("count = %d, err=%v", count, err)
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("close pool: %v", err)
	}
	stop()
	startPizzaKVTest(t, binary, socket, database)
	pool = waitPizzaKVPool(t, socket)
	defer pool.Close()
	schemas = NewSchemaManager(pool, "integration")
	tables = NewTableManager(pool, schemas, "integration")
	row, err = tables.GetByPK("events", "1")
	if err != nil {
		t.Fatalf("point read after restart: %v", err)
	}
	if got, ok := row["payload"].([]byte); !ok || !bytes.Equal(got, []byte{1, 0, '|', '\r', '\n'}) {
		t.Fatalf("binary payload after restart = %v (%T)", row["payload"], row["payload"])
	}
	if count, err := tables.CountFast("events"); err != nil || count != 21 {
		t.Fatalf("count after restart = %d, err=%v", count, err)
	}
	if err := schemas.DropTable("events"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if schemas.TableExists("events") {
		t.Fatal("table still exists")
	}
}

func TestPizzaKVLegacyMigrationIntegration(t *testing.T) {
	binary := os.Getenv("PIZZAKV_BIN")
	if binary == "" {
		t.Skip("PIZZAKV_BIN is not set")
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.db")
	destination := filepath.Join(dir, "legacy.pkvdb")
	socket := shortPizzaKVSocket(t)
	schema, err := json.Marshal(&Schema{
		Name:       "events",
		Columns:    []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}, {Name: "name", Type: "TEXT", Nullable: true}},
		PrimaryKey: "id",
		NextRowID:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(fmt.Sprintf(
		"W|legacy:_schema:events|%s\rW|legacy:_sys:tables|[\"events\"]\rW|legacy:_data:events:1|{\"id\":1,\"name\":\"old\",\"_rowid_\":1}\r",
		schema,
	))
	if err := os.WriteFile(source, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(binary, "-migrate="+source, "-path="+destination).CombinedOutput(); err != nil {
		t.Fatalf("migrate legacy database: %v\n%s", err, output)
	}
	unchanged, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, legacy) {
		t.Fatal("migration modified the legacy source")
	}

	startPizzaKVTest(t, binary, socket, destination)
	pool := waitPizzaKVPool(t, socket)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "legacy")
	tables := NewTableManager(pool, schemas, "legacy")
	row, err := tables.GetByPK("events", "1")
	if err != nil || row["name"] != "old" {
		t.Fatalf("read migrated row: row=%v err=%v", row, err)
	}
	if err := tables.Insert("events", Row{"id": int64(2), "name": "new"}); err != nil {
		t.Fatalf("insert binary row after migration: %v", err)
	}
	rows, err := tables.Select("events", nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("mixed legacy/binary scan: len=%d err=%v", len(rows), err)
	}
}

func BenchmarkPizzaKVStorage(b *testing.B) {
	binary := os.Getenv("PIZZAKV_BIN")
	if binary == "" {
		b.Skip("PIZZAKV_BIN is not set")
	}

	dir := b.TempDir()
	socket := shortPizzaKVSocket(b)
	startPizzaKVTest(b, binary, socket, filepath.Join(dir, "benchmark.pkvdb"))
	pool := waitPizzaKVPool(b, socket)
	defer pool.Close()
	schemas := NewSchemaManager(pool, "benchmark")
	tables := NewTableManager(pool, schemas, "benchmark")
	if err := schemas.CreateTable(&Schema{
		Name: "events",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "symbol", Type: "TEXT", Nullable: false},
			{Name: "price", Type: "REAL", Nullable: false},
		},
	}); err != nil {
		b.Fatal(err)
	}
	rows := make([]Row, 10_000)
	for i := range rows {
		rows[i] = Row{"id": int64(i + 1), "symbol": fmt.Sprintf("PIZZA-%03d", i%100), "price": float64(i) / 100}
	}
	if n, err := tables.InsertBulk("events", rows); err != nil || n != len(rows) {
		b.Fatalf("seed: n=%d err=%v", n, err)
	}

	b.Run("point_read", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := tables.GetByPK("events", "5000"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("limit_10", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := tables.SelectWithLimit("events", nil, 10, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("scan_10000", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if rows, err := tables.Select("events", nil); err != nil || len(rows) != 10_000 {
				b.Fatalf("scan: len=%d err=%v", len(rows), err)
			}
		}
	})
}
