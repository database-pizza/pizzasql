package pgserver

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

// connectPGXPair starts one wire server and returns two independent client
// connections to the same database so row-lock contention can be exercised.
func connectPGXPair(t *testing.T) (*pgx.Conn, *pgx.Conn) {
	t.Helper()
	conns := connectPGXMany(t, 2)
	return conns[0], conns[1]
}

func connectPGXMany(t *testing.T, n int) []*pgx.Conn {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(8)
	t.Cleanup(func() { pool.Close() })
	dbManager := storage.NewDatabaseManager(pool, &storage.DatabaseManagerConfig{
		DefaultDatabase: "rowlocks",
		AutoCreate:      true,
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	go func() {
		for {
			server, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = NewConnection(server, dbManager, true).Handle()
			}()
		}
	}()

	dsn := "postgres://tester@" + listener.Addr().String() + "/rowlocks?sslmode=disable"
	connect := func() *pgx.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("pgx connect: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		return conn
	}
	conns := make([]*pgx.Conn, 0, n)
	for i := 0; i < n; i++ {
		conns = append(conns, connect())
	}
	return conns
}

func TestRowLockSkipLockedClaimsOneRowPerTransaction(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	for conn, statements := range map[*pgx.Conn][]string{
		a: {
			`CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT, owner TEXT)`,
			`INSERT INTO jobs VALUES (1, 'pending', NULL), (2, 'pending', NULL), (3, 'pending', NULL)`,
		},
	} {
		for _, sql := range statements {
			if _, err := conn.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}

	claim := func(conn *pgx.Conn, owner string) int32 {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var id int32
		if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id); err != nil {
			t.Fatalf("%s claim: %v", owner, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'taken', owner = $1 WHERE id = $2`, owner, id); err != nil {
			t.Fatalf("%s update: %v", owner, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("%s commit: %v", owner, err)
		}
		return id
	}

	// The first transaction keeps its lock open while the second claims.
	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var first int32
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&first); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first != 1 {
		t.Fatalf("first claim = %d, want 1", first)
	}

	second := claim(b, "b")
	if second == first {
		t.Fatalf("both transactions claimed row %d", first)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'taken', owner = 'a' WHERE id = $1`, first); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	rows, err := a.Query(ctx, `SELECT id, status, owner FROM jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	taken := 0
	for rows.Next() {
		var id int32
		var status string
		var owner *string
		if err := rows.Scan(&id, &status, &owner); err != nil {
			t.Fatal(err)
		}
		if status == "taken" {
			if owner == nil {
				t.Fatalf("row %d is taken without an owner", id)
			}
			taken++
		} else if id != 3 || status != "pending" {
			t.Fatalf("row %d = %s/%v, want pending or taken", id, status, owner)
		}
		count++
	}
	if count != 3 || taken != 2 {
		t.Fatalf("rows/taken = %d/%d, want 3/2", count, taken)
	}
}

func TestRowLockNowaitAndWriterConflict(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending'), (2, 'pending')`); err != nil {
		t.Fatal(err)
	}

	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	_, err = b.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE NOWAIT`)
	if err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("NOWAIT error = %v, want SQLSTATE 55P03", err)
	}

	// A writer that does not use a locking clause must not commit over the
	// lock; it gets the optimistic serialization failure.
	_, err = b.Exec(ctx, `UPDATE jobs SET status = 'stolen' WHERE id = 1`)
	if err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("writer error = %v, want SQLSTATE 40001", err)
	}
}

func TestRowLockBlockingWaitsForCommit(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending')`); err != nil {
		t.Fatal(err)
	}

	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		var id int32
		err := b.QueryRow(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`).Scan(&id)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("blocking FOR UPDATE returned before commit: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocking FOR UPDATE: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking FOR UPDATE did not resume")
	}
}

func TestRowLockDeadlockDetected(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}

	ta, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.Rollback(ctx)
	tb, err := b.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Rollback(ctx)
	if _, err := ta.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Exec(ctx, `SELECT id FROM jobs WHERE id = 2 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		_, err := ta.Exec(ctx, `SELECT id FROM jobs WHERE id = 2 FOR UPDATE`)
		blocked <- err
	}()
	time.Sleep(100 * time.Millisecond)

	_, err = tb.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`)
	if err == nil || !strings.Contains(err.Error(), "40P01") {
		t.Fatalf("deadlock error = %v, want SQLSTATE 40P01", err)
	}
	if err := tb.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("waiting transaction after deadlock peer rollback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting transaction did not resume")
	}
}

func TestRowLockShareAllowsReadersAndBlocksWriter(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending')`); err != nil {
		t.Fatal(err)
	}

	ta, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.Rollback(ctx)
	tb, err := b.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Rollback(ctx)

	if _, err := ta.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR SHARE`); err != nil {
		t.Fatalf("second FOR SHARE: %v", err)
	}
	if _, err := tb.Exec(ctx, `UPDATE jobs SET status = 'taken' WHERE id = 1`); err != nil {
		t.Fatalf("staging writer: %v", err)
	}
	err = tb.Commit(ctx)
	if err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("writer with share lock held error = %v, want 40001", err)
	}
}

func TestRowLockAutocommitReleasesImmediately(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `UPDATE jobs SET status = 'taken' WHERE id = 1`); err != nil {
		t.Fatalf("autocommit lock was not released: %v", err)
	}
}

func TestRowLockQueueWorkersDoNotDoubleClaim(t *testing.T) {
	const workers = 4
	const jobs = 40
	conns := connectPGXMany(t, workers)
	ctx := context.Background()
	if _, err := conns[0].Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT, owner TEXT)`); err != nil {
		t.Fatal(err)
	}
	var insert strings.Builder
	insert.WriteString(`INSERT INTO jobs VALUES `)
	for i := 1; i <= jobs; i++ {
		if i > 1 {
			insert.WriteString(", ")
		}
		fmt.Fprintf(&insert, "(%d, 'pending', NULL)", i)
	}
	if _, err := conns[0].Exec(ctx, insert.String()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w, conn := range conns {
		wg.Add(1)
		go func(w int, conn *pgx.Conn) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", w)
			for {
				tx, err := conn.Begin(ctx)
				if err != nil {
					errs <- err
					return
				}
				var id int32
				err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE status = 'pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
				if err != nil {
					_ = tx.Rollback(ctx)
					if err == pgx.ErrNoRows {
						return
					}
					errs <- err
					return
				}
				if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'taken', owner = $1 WHERE id = $2`, owner, id); err != nil {
					_ = tx.Rollback(ctx)
					errs <- err
					return
				}
				if err := tx.Commit(ctx); err != nil {
					errs <- err
					return
				}
			}
		}(w, conn)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("worker error: %v", err)
	}

	var taken, pending int
	if err := conns[0].QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'taken'`).Scan(&taken); err != nil {
		t.Fatal(err)
	}
	if err := conns[0].QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if taken != jobs || pending != 0 {
		t.Fatalf("taken/pending = %d/%d, want %d/0", taken, pending, jobs)
	}
	var distinctOwners int
	if err := conns[0].QueryRow(ctx, `SELECT COUNT(DISTINCT id) FROM jobs`).Scan(&distinctOwners); err != nil {
		t.Fatal(err)
	}
	if distinctOwners != jobs {
		t.Fatalf("distinct ids = %d, want %d", distinctOwners, jobs)
	}
}

func TestRowLockAliasedTableAndEmptySkip(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending'), (2, 'pending')`); err != nil {
		t.Fatal(err)
	}

	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id int32
	if err := tx.QueryRow(ctx, `SELECT j.id FROM jobs AS j WHERE j.id = 1 FOR UPDATE`).Scan(&id); err != nil {
		t.Fatalf("aliased locking select: %v", err)
	}
	if id != 1 {
		t.Fatalf("aliased id = %d", id)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Hold both rows, then confirm SKIP LOCKED reports no candidate.
	ta, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ta.Rollback(ctx)
	if _, err := ta.Exec(ctx, `SELECT id FROM jobs FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	tb, err := b.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Rollback(ctx)
	var n int32
	if err := tb.QueryRow(ctx, `SELECT id FROM jobs WHERE status = 'pending' LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&n); err != pgx.ErrNoRows {
		t.Fatalf("all-locked SKIP LOCKED = %v, want ErrNoRows", err)
	}
}

func TestRowLockRollbackReleases(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending')`); err != nil {
		t.Fatal(err)
	}
	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `UPDATE jobs SET status = 'taken' WHERE id = 1`); err != nil {
		t.Fatalf("rollback did not release lock: %v", err)
	}
}

func TestRowLockBlocksAutocommitDDL(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending')`); err != nil {
		t.Fatal(err)
	}
	tx, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM jobs FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `ALTER TABLE jobs ADD COLUMN note TEXT`); err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("DDL with row lock held error = %v, want 40001", err)
	}
}

func TestRowLockSyntheticRowIDTable(t *testing.T) {
	a, _ := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE notes (body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO notes VALUES ('hello')`); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := a.QueryRow(ctx, `SELECT body FROM notes FOR UPDATE`).Scan(&body); err != nil {
		t.Fatalf("locking synthetic-rowid table: %v", err)
	}
	if body != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestRowLockSkippedPrimaryKeyDoesNotPoisonCommit(t *testing.T) {
	a, b := connectPGXPair(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, `INSERT INTO jobs VALUES (1, 'pending'), (2, 'pending')`); err != nil {
		t.Fatal(err)
	}

	ta, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ta.Exec(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	tb, err := b.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id int32
	if err := tb.QueryRow(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE SKIP LOCKED`).Scan(&id); err != pgx.ErrNoRows {
		t.Fatalf("skipped primary key = %v, want ErrNoRows", err)
	}
	if _, err := tb.Exec(ctx, `UPDATE jobs SET status = 'b' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}

	if _, err := ta.Exec(ctx, `UPDATE jobs SET status = 'a' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := ta.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tb.Commit(ctx); err != nil {
		t.Fatalf("skipped read poisoned commit: %v", err)
	}
}
