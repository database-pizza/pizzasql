package storage

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Row-lock contention outcomes. ErrRowLocked is intentionally reported to the
// client as a serialization failure (40001): the optimistic commit model makes
// a conflicting writer retry rather than block.
var (
	ErrRowLocked        = errors.New("row is locked by another transaction")
	ErrLockNotAvailable = errors.New("row lock is not available")
	ErrDeadlockDetected = errors.New("deadlock detected")
)

// LockMode is the strength of a locking read.
type LockMode uint8

const (
	LockUpdate LockMode = iota
	LockShare
)

// LockWait is the contention policy of a locking read.
type LockWait uint8

const (
	LockWaitBlock LockWait = iota
	LockWaitNoWait
	LockWaitSkip
)

type rowLockEntry struct {
	mu     sync.Mutex
	closed bool
	owners map[*Session]LockMode
	// waiters are signalled when an owner releases. A waiter retries the
	// acquisition instead of receiving ownership directly: a release can wake
	// several compatible readers.
	waiters map[chan struct{}]struct{}
}

func (e *rowLockEntry) ownerList() []*Session {
	owners := make([]*Session, 0, len(e.owners))
	for owner := range e.owners {
		owners = append(owners, owner)
	}
	return owners
}

// RowLockManager holds in-process row locks. Locking reads create entries;
// ordinary writes do not. A write checks the entries at its durable batch
// boundary and fails with ErrSerialization if another transaction holds one.
//
// gate is a reader/writer gate around lock insertion checks and the write-side
// check. A write holds it for reading from the check through publication, so a
// lock cannot be inserted in the window between "no lock seen" and "committed".
// Waiting for a row lock never holds gate.
type RowLockManager struct {
	gate    sync.RWMutex
	entries sync.Map // string (data key) -> *rowLockEntry
	active  atomic.Int64

	waitMu sync.Mutex
	waits  map[*Session]*Session
}

func NewRowLockManager() *RowLockManager {
	return &RowLockManager{waits: make(map[*Session]*Session)}
}

// Active reports whether any row locks are currently held. Callers use it to
// skip the map scan on the normal, lock-free path.
func (l *RowLockManager) Active() bool {
	l.gate.RLock()
	defer l.gate.RUnlock()
	return l.active.Load() > 0
}

func (l *RowLockManager) entry(key string) (*rowLockEntry, bool) {
	value, ok := l.entries.Load(key)
	if !ok {
		return nil, false
	}
	return value.(*rowLockEntry), true
}

// tryLock acquires key if it is free or compatible. The caller must hold gate.
func (l *RowLockManager) tryLock(owner *Session, key string, mode LockMode) (bool, *Session) {
	e, ok := l.entry(key)
	if !ok {
		e = &rowLockEntry{owners: make(map[*Session]LockMode), waiters: make(map[chan struct{}]struct{})}
		e.owners[owner] = mode
		l.entries.Store(key, e)
		l.active.Add(1)
		return true, nil
	}
	e.mu.Lock()
	if e.closed {
		// A concurrent Release deleted this entry after we loaded it. We hold
		// gate for writing, so it is safe to replace it with a fresh entry.
		e.mu.Unlock()
		e = &rowLockEntry{owners: make(map[*Session]LockMode), waiters: make(map[chan struct{}]struct{})}
		e.owners[owner] = mode
		l.entries.Store(key, e)
		l.active.Add(1)
		return true, nil
	}
	defer e.mu.Unlock()
	if current, held := e.owners[owner]; held {
		if current == LockUpdate || mode == LockShare {
			return true, nil
		}
		// Upgrade FOR SHARE to FOR UPDATE only when no other reader holds one.
		if len(e.owners) == 1 {
			e.owners[owner] = LockUpdate
			return true, nil
		}
		for other := range e.owners {
			return false, other
		}
	}
	if len(e.owners) == 0 {
		e.owners[owner] = mode
		return true, nil
	}
	if mode == LockShare {
		compatible := true
		var other *Session
		for holder, held := range e.owners {
			if held == LockUpdate {
				compatible = false
				other = holder
				break
			}
		}
		if compatible {
			e.owners[owner] = LockShare
			return true, nil
		}
		return false, other
	}
	for other := range e.owners {
		return false, other
	}
	return false, nil
}

// Acquire obtains a row lock. Skip and NoWait return immediately when the row is
// contended; Block waits until it is free and reports a deadlock as
// ErrDeadlockDetected.
func (l *RowLockManager) Acquire(owner *Session, key string, mode LockMode, wait LockWait) error {
	for {
		l.gate.Lock()
		ok, other := l.tryLock(owner, key, mode)
		l.gate.Unlock()
		if ok {
			return nil
		}
		switch wait {
		case LockWaitNoWait:
			return ErrLockNotAvailable
		case LockWaitSkip:
			return ErrRowLocked
		}
		if err := l.waitFor(owner, key, other); err != nil {
			return err
		}
	}
}

func (l *RowLockManager) waitFor(owner *Session, key string, other *Session) error {
	e, ok := l.entry(key)
	if !ok {
		return nil
	}
	ch := make(chan struct{})
	e.mu.Lock()
	// The owner may have released and removed the entry after tryLock returned
	// but before we enqueued. Re-check under the entry lock so we never sleep on
	// a detached entry.
	current, present := l.entry(key)
	if !present || current != e || len(e.owners) == 0 {
		e.mu.Unlock()
		return nil
	}
	e.waiters[ch] = struct{}{}
	e.mu.Unlock()

	l.waitMu.Lock()
	l.waits[owner] = other
	deadlock := l.hasCycleLocked(owner)
	if deadlock {
		delete(l.waits, owner)
	} else {
		l.waits[owner] = other
	}
	l.waitMu.Unlock()
	if deadlock {
		e.mu.Lock()
		delete(e.waiters, ch)
		e.mu.Unlock()
		return ErrDeadlockDetected
	}
	defer func() {
		l.waitMu.Lock()
		if l.waits[owner] == other {
			delete(l.waits, owner)
		}
		l.waitMu.Unlock()
	}()

	select {
	case <-ch:
		return nil
	// A genuine cycle is detected by the wait-for graph immediately; this is a
	// last-resort guard against a lost wake-up, not a lock timeout.
	case <-time.After(5 * time.Minute):
		e.mu.Lock()
		delete(e.waiters, ch)
		e.mu.Unlock()
		return ErrDeadlockDetected
	}
}

func (l *RowLockManager) hasCycleLocked(start *Session) bool {
	seen := map[*Session]bool{}
	cur := start
	for cur != nil {
		if seen[cur] {
			return cur == start
		}
		seen[cur] = true
		cur = l.waits[cur]
	}
	return false
}

// Release drops one owner's hold on key and wakes waiters.
func (l *RowLockManager) Release(owner *Session, key string) {
	e, ok := l.entry(key)
	if !ok {
		return
	}
	e.mu.Lock()
	delete(e.owners, owner)
	empty := len(e.owners) == 0
	waiters := make([]chan struct{}, 0, len(e.waiters))
	if empty || len(e.owners) == 0 {
		for ch := range e.waiters {
			waiters = append(waiters, ch)
		}
		e.waiters = make(map[chan struct{}]struct{})
	}
	if empty {
		e.closed = true
		l.entries.Delete(key)
		l.active.Add(-1)
	}
	e.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// ReleaseAll drops every lock held by owner. It is used at transaction end,
// rollback, and failed commit. It intentionally does not take gate: releasing a
// lock can never invalidate an in-flight commit, and resetLocked may call it
// while a commit still holds gate for reading.
func (l *RowLockManager) ReleaseAll(owner *Session) {
	l.entries.Range(func(key, value interface{}) bool {
		e := value.(*rowLockEntry)
		e.mu.Lock()
		delete(e.owners, owner)
		empty := len(e.owners) == 0
		var waiters []chan struct{}
		if empty {
			e.closed = true
			waiters = make([]chan struct{}, 0, len(e.waiters))
			for ch := range e.waiters {
				waiters = append(waiters, ch)
			}
			e.waiters = make(map[chan struct{}]struct{})
			l.entries.Delete(key)
			l.active.Add(-1)
		}
		e.mu.Unlock()
		for _, ch := range waiters {
			close(ch)
		}
		return true
	})
	l.waitMu.Lock()
	delete(l.waits, owner)
	l.waitMu.Unlock()
}

// BeginWrite holds the commit gate for reading and verifies that no other
// transaction holds any of keys. The returned function must be called after the
// durable batch is published. Holding gate across publication is what makes a
// lock acquisition and a write mutually exclusive.
func (l *RowLockManager) BeginWrite(owner *Session, keys, tablePrefixes []string) (func(), error) {
	l.gate.RLock()
	if l.active.Load() == 0 {
		return l.gate.RUnlock, nil
	}
	for _, key := range keys {
		e, ok := l.entry(key)
		if !ok {
			continue
		}
		e.mu.Lock()
		conflict := false
		for holder := range e.owners {
			if holder != owner {
				conflict = true
				break
			}
		}
		e.mu.Unlock()
		if conflict {
			l.gate.RUnlock()
			return nil, ErrRowLocked
		}
	}
	if len(tablePrefixes) > 0 {
		conflict := false
		l.entries.Range(func(key, value interface{}) bool {
			k := key.(string)
			matched := false
			for _, prefix := range tablePrefixes {
				if strings.HasPrefix(k, prefix) {
					matched = true
					break
				}
			}
			if !matched {
				return true
			}
			e := value.(*rowLockEntry)
			e.mu.Lock()
			for holder := range e.owners {
				if holder != owner {
					conflict = true
					break
				}
			}
			e.mu.Unlock()
			return !conflict
		})
		if conflict {
			l.gate.RUnlock()
			return nil, ErrRowLocked
		}
	}
	return l.gate.RUnlock, nil
}

// BeginTableWrite is the coarse check used by autocommit bulk/DDL operations
// whose exact row keys are not known before the scan. It rejects the write if
// any other transaction holds a row lock in the table.
func (l *RowLockManager) BeginTableWrite(owner *Session, prefix string) (func(), error) {
	return l.BeginWrite(owner, nil, []string{prefix})
}
