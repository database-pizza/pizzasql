package executor

import (
	"fmt"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// listenOp records one LISTEN/UNLISTEN statement so its effect can be applied
// at transaction commit. PostgreSQL makes subscription changes visible only
// after commit; a rollback leaves the previous subscription set untouched.
type listenOp struct {
	kind    listenOpKind
	channel string
}

type listenOpKind int

const (
	listenOpListen listenOpKind = iota
	listenOpUnlisten
	listenOpUnlistenAll
)

// notificationSnapshot captures the staged notification state at a savepoint.
type notificationSnapshot struct {
	listenOps   int
	notifyQueue int
}

// SetNotificationHub installs the per-database notification registry and
// creates this session's subscriber. It must be called before the executor
// processes LISTEN or NOTIFY. Safe to call once per connection.
func (e *Executor) SetNotificationHub(hub *storage.Hub) {
	e.hub = hub
	if hub == nil {
		return
	}
	if e.subscriber == nil {
		e.subscriber = storage.NewSubscriber(e.backendPID, 0)
	}
}

// SetBackendPID sets the backend PID reported in NotificationResponse frames.
// It also applies to an already-created subscriber.
func (e *Executor) SetBackendPID(pid int32) {
	e.backendPID = pid
	if e.subscriber == nil && e.hub != nil {
		e.subscriber = storage.NewSubscriber(pid, 0)
	}
}

// NotificationChannel returns the receive side of this session's notification
// queue, or nil when no hub is configured.
func (e *Executor) NotificationChannel() <-chan storage.Notification {
	if e.subscriber == nil {
		return nil
	}
	return e.subscriber.Notifications()
}

// NotificationOverflow is closed when the session's outbound queue is full and
// the hub has dropped it. Protocol layers should disconnect the connection.
func (e *Executor) NotificationOverflow() <-chan struct{} {
	if e.subscriber == nil {
		return nil
	}
	return e.subscriber.Overflow()
}

// Close releases this session's subscriptions. It is the disconnect path; it
// is safe to call more than once.
func (e *Executor) Close() error {
	if e.hub != nil && e.subscriber != nil {
		e.hub.UnsubscribeAll(e.subscriber)
	}
	e.listens = make(map[string]bool)
	e.listenOps = nil
	e.notifyQueue = nil
	e.notificationSaves = nil
	return e.RollbackActive()
}

func (e *Executor) executeListen(stmt *parser.ListenStmt) (*Result, error) {
	if stmt.Channel == "" {
		return nil, fmt.Errorf("channel name cannot be empty")
	}
	if e.inTransaction {
		e.listenOps = append(e.listenOps, listenOp{kind: listenOpListen, channel: stmt.Channel})
		return NewResult("LISTEN"), nil
	}
	e.applyListenOp(listenOp{kind: listenOpListen, channel: stmt.Channel})
	return NewResult("LISTEN"), nil
}

func (e *Executor) executeUnlisten(stmt *parser.UnlistenStmt) (*Result, error) {
	op := listenOp{kind: listenOpUnlisten, channel: stmt.Channel}
	if stmt.All {
		op = listenOp{kind: listenOpUnlistenAll}
	} else if stmt.Channel == "" {
		return nil, fmt.Errorf("channel name cannot be empty")
	}
	if e.inTransaction {
		e.listenOps = append(e.listenOps, op)
		return NewResult("UNLISTEN"), nil
	}
	e.applyListenOp(op)
	return NewResult("UNLISTEN"), nil
}

func (e *Executor) executeNotify(stmt *parser.NotifyStmt) (*Result, error) {
	if len(stmt.Payload) > 8000 {
		return nil, fmt.Errorf("notification payload is longer than 8000 bytes")
	}
	n := storage.Notification{
		PID:     e.backendPID,
		Channel: stmt.Channel,
		Payload: stmt.Payload,
	}
	if !e.inTransaction {
		e.publish(n)
		return NewResult("NOTIFY"), nil
	}
	// PostgreSQL collapses duplicate (channel, payload) pairs within one
	// transaction, regardless of how many times they were raised.
	for _, queued := range e.notifyQueue {
		if queued.Channel == n.Channel && queued.Payload == n.Payload {
			return NewResult("NOTIFY"), nil
		}
	}
	e.notifyQueue = append(e.notifyQueue, n)
	return NewResult("NOTIFY"), nil
}

func (e *Executor) applyListenOp(op listenOp) {
	if e.hub == nil || e.subscriber == nil {
		return
	}
	switch op.kind {
	case listenOpListen:
		if !e.listens[op.channel] {
			e.hub.Subscribe(e.subscriber, op.channel)
			e.listens[op.channel] = true
		}
	case listenOpUnlisten:
		if e.listens[op.channel] {
			e.hub.Unsubscribe(e.subscriber, op.channel)
			delete(e.listens, op.channel)
		}
	case listenOpUnlistenAll:
		for channel := range e.listens {
			e.hub.Unsubscribe(e.subscriber, channel)
		}
		e.listens = make(map[string]bool)
	}
}

func (e *Executor) publish(n storage.Notification) {
	if e.hub == nil {
		return
	}
	e.hub.Publish(n)
}

// finishCommit applies staged subscription changes and releases queued
// notifications. It runs only after the storage commit has succeeded.
func (e *Executor) finishCommit(lsn uint64) {
	for _, op := range e.listenOps {
		e.applyListenOp(op)
	}
	for _, n := range e.notifyQueue {
		n.LSN = lsn
		e.publish(n)
	}
	e.clearNotificationState()
}

// finishRollback discards staged subscription changes and notifications.
func (e *Executor) finishRollback() {
	e.clearNotificationState()
}

func (e *Executor) clearNotificationState() {
	e.listenOps = nil
	e.notifyQueue = nil
	e.notificationSaves = nil
}

func (e *Executor) notificationSnapshot() notificationSnapshot {
	return notificationSnapshot{
		listenOps:   len(e.listenOps),
		notifyQueue: len(e.notifyQueue),
	}
}

func (e *Executor) rollbackNotificationsTo(s notificationSnapshot) {
	if s.listenOps <= len(e.listenOps) {
		e.listenOps = e.listenOps[:s.listenOps]
	}
	if s.notifyQueue <= len(e.notifyQueue) {
		e.notifyQueue = e.notifyQueue[:s.notifyQueue]
	}
}
