package executor

import (
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

func newNotifyExec(t *testing.T, hub *storage.Hub, schema *storage.SchemaManager, table *storage.TableManager, pid int32) *Executor {
	t.Helper()
	e := newExec(schema, table)
	e.SetBackendPID(pid)
	e.SetNotificationHub(hub)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func recvNotification(t *testing.T, e *Executor) storage.Notification {
	t.Helper()
	select {
	case n := <-e.NotificationChannel():
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
		return storage.Notification{}
	}
}

func expectNoNotification(t *testing.T, e *Executor) {
	t.Helper()
	select {
	case n := <-e.NotificationChannel():
		t.Fatalf("unexpected notification: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyDeliversToListener(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN updates")
	execMust(t, sender, "NOTIFY updates, 'hello'")

	n := recvNotification(t, listener)
	if n.Channel != "updates" || n.Payload != "hello" || n.PID != 202 {
		t.Fatalf("unexpected notification: %+v", n)
	}
}

func TestListenTakesEffectAtCommit(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "BEGIN")
	execMust(t, listener, "LISTEN late")
	execMust(t, sender, "NOTIFY late, 'before'")
	expectNoNotification(t, listener)

	execMust(t, listener, "COMMIT")
	execMust(t, sender, "NOTIFY late, 'after'")
	n := recvNotification(t, listener)
	if n.Payload != "after" {
		t.Fatalf("payload = %q, want after", n.Payload)
	}
}

func TestListenRollbackDiscardsSubscription(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "BEGIN")
	execMust(t, listener, "LISTEN rolledback")
	execMust(t, listener, "ROLLBACK")
	execMust(t, sender, "NOTIFY rolledback, 'nope'")
	expectNoNotification(t, listener)
}

func TestNotifyOnlyDeliveredAfterCommit(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN events")
	execMust(t, sender, "BEGIN")
	execMust(t, sender, "NOTIFY events, 'pending'")
	expectNoNotification(t, listener)
	execMust(t, sender, "COMMIT")

	n := recvNotification(t, listener)
	if n.Payload != "pending" || n.LSN == 0 {
		t.Fatalf("unexpected notification: %+v", n)
	}
}

func TestNotifyRollbackDiscards(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN events")
	execMust(t, sender, "BEGIN")
	execMust(t, sender, "NOTIFY events, 'gone'")
	execMust(t, sender, "ROLLBACK")
	expectNoNotification(t, listener)
}

func TestNotifySavepointRollback(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN events")
	execMust(t, sender, "BEGIN")
	execMust(t, sender, "NOTIFY events, 'kept'")
	execMust(t, sender, "SAVEPOINT sp")
	execMust(t, sender, "NOTIFY events, 'discarded'")
	execMust(t, sender, "ROLLBACK TO SAVEPOINT sp")
	execMust(t, sender, "COMMIT")

	n := recvNotification(t, listener)
	if n.Payload != "kept" {
		t.Fatalf("payload = %q, want kept", n.Payload)
	}
	expectNoNotification(t, listener)
}

func TestNotifyDuplicatesCollapseInTransaction(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN events")
	execMust(t, sender, "BEGIN")
	execMust(t, sender, "NOTIFY events, 'same'")
	execMust(t, sender, "NOTIFY events, 'same'")
	execMust(t, sender, "NOTIFY events, 'different'")
	execMust(t, sender, "COMMIT")

	first := recvNotification(t, listener)
	second := recvNotification(t, listener)
	if first.Payload != "same" || second.Payload != "different" {
		t.Fatalf("unexpected notifications: %+v %+v", first, second)
	}
	expectNoNotification(t, listener)
}

func TestUnlistenAllAndDisconnectCleanup(t *testing.T) {
	_, schema, table := newTestDB(t)
	hub := storage.NewHub()
	listener := newNotifyExec(t, hub, schema, table, 101)
	sender := newNotifyExec(t, hub, schema, table, 202)

	execMust(t, listener, "LISTEN a")
	execMust(t, listener, "LISTEN b")
	execMust(t, listener, "UNLISTEN *")
	execMust(t, sender, "NOTIFY a, 'x'")
	execMust(t, sender, "NOTIFY b, 'y'")
	expectNoNotification(t, listener)

	execMust(t, listener, "LISTEN c")
	_ = listener.Close()
	execMust(t, sender, "NOTIFY c, 'z'")
	expectNoNotification(t, listener)
}

func TestCommitLSNIsMonotonic(t *testing.T) {
	_, schema, table := newTestDB(t)
	e := newExec(schema, table)
	execMust(t, e, "CREATE TABLE lsn_test (id INTEGER PRIMARY KEY, v TEXT)")

	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO lsn_test VALUES (1, 'a')")
	first := execMust(t, e, "COMMIT")
	if first.LSN == 0 {
		t.Fatal("first commit LSN is zero")
	}

	execMust(t, e, "BEGIN")
	execMust(t, e, "INSERT INTO lsn_test VALUES (2, 'b')")
	second := execMust(t, e, "COMMIT")
	if second.LSN <= first.LSN {
		t.Fatalf("second commit LSN %d did not advance past %d", second.LSN, first.LSN)
	}
}
