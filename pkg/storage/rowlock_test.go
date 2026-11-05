package storage

import (
	"errors"
	"testing"
	"time"
)

func TestRowLocksModesAndWriteGuard(t *testing.T) {
	m := NewRowLockManager()
	a, b, c := &Session{}, &Session{}, &Session{}
	key := "db:_data:t:1"

	if err := m.Acquire(a, key, LockUpdate, LockWaitBlock); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(b, key, LockShare, LockWaitSkip); !errors.Is(err, ErrRowLocked) {
		t.Fatalf("contended share = %v, want ErrRowLocked", err)
	}
	if err := m.Acquire(b, key, LockShare, LockWaitNoWait); !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("contended nowait = %v, want ErrLockNotAvailable", err)
	}
	if _, err := m.BeginWrite(b, []string{key}, nil); !errors.Is(err, ErrRowLocked) {
		t.Fatalf("write with another owner's lock = %v, want ErrRowLocked", err)
	}
	release, err := m.BeginWrite(a, []string{key}, nil)
	if err != nil {
		t.Fatalf("owner write guard: %v", err)
	}
	release()
	m.Release(a, key)

	if err := m.Acquire(a, key, LockShare, LockWaitBlock); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(b, key, LockShare, LockWaitBlock); err != nil {
		t.Fatalf("compatible share: %v", err)
	}
	if err := m.Acquire(c, key, LockUpdate, LockWaitNoWait); !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("update over shares = %v, want ErrLockNotAvailable", err)
	}
	m.ReleaseAll(a)
	if err := m.Acquire(c, key, LockUpdate, LockWaitNoWait); !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("update with one share left = %v, want ErrLockNotAvailable", err)
	}
	m.ReleaseAll(b)
	if m.Active() {
		t.Fatal("locks remain active after ReleaseAll")
	}
}

func TestRowLockBlockingReleaseWakesWaiter(t *testing.T) {
	m := NewRowLockManager()
	a, b := &Session{}, &Session{}
	key := "db:_data:t:2"
	if err := m.Acquire(a, key, LockUpdate, LockWaitBlock); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Acquire(b, key, LockUpdate, LockWaitBlock) }()
	select {
	case err := <-done:
		t.Fatalf("waiter returned before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.ReleaseAll(a)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake")
	}
	m.ReleaseAll(b)
}
