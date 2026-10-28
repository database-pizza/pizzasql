package storage

import (
	"sync"
	"sync/atomic"
)

// Notification is a single NOTIFY event delivered to subscribers of a channel.
// Payload is the application-supplied text (at most 8000 bytes), PID is the
// backend that raised it, and LSN is the commit LSN of the transaction that
// made it visible (zero for a NOTIFY outside a write transaction).
type Notification struct {
	PID     int32
	Channel string
	Payload string
	LSN     uint64
}

// Subscriber is one connection's view of the notification hub. A subscriber
// may listen to many channels and is registered with exactly one Hub.
type Subscriber struct {
	id int32

	// notifications is bounded so a slow client can never block a publisher.
	// Hub.Publish drops the whole subscriber (closing overflow) instead of
	// waiting when the buffer is full.
	notifications chan Notification
	overflow      chan struct{}
	overflowOnce  sync.Once
}

// NewSubscriber creates a subscriber with the given backend PID and an
// outbound queue of queueSize notifications.
func NewSubscriber(pid int32, queueSize int) *Subscriber {
	if queueSize <= 0 {
		queueSize = defaultNotificationQueue
	}
	return &Subscriber{
		id:            pid,
		notifications: make(chan Notification, queueSize),
		overflow:      make(chan struct{}),
	}
}

const defaultNotificationQueue = 128

// Notifications returns the channel notifications are delivered on.
func (s *Subscriber) Notifications() <-chan Notification { return s.notifications }

// Overflow is closed when the subscriber could not keep up and has been
// removed from the hub. Protocol layers should disconnect the connection.
func (s *Subscriber) Overflow() <-chan struct{} { return s.overflow }

// PID returns the backend PID associated with the subscriber.
func (s *Subscriber) PID() int32 { return s.id }

func (s *Subscriber) markOverflow() {
	s.overflowOnce.Do(func() { close(s.overflow) })
}

// Hub is a per-database channel registry. It maps channel names to subscribed
// connections and broadcasts notifications without blocking publishers. A Hub
// is scoped to one DatabaseInstance, so tenants sharing a process never observe
// each other's channels.
type Hub struct {
	mu          sync.RWMutex
	channels    map[string]map[*Subscriber]struct{}
	subscribers map[*Subscriber]struct{}
	published   atomic.Uint64
	dropped     atomic.Uint64
	lastLSN     atomic.Uint64
}

// NewHub creates an empty notification hub.
func NewHub() *Hub {
	return &Hub{
		channels:    make(map[string]map[*Subscriber]struct{}),
		subscribers: make(map[*Subscriber]struct{}),
	}
}

// Subscribe registers a subscriber for a channel. Duplicate subscriptions are
// idempotent.
func (h *Hub) Subscribe(s *Subscriber, channel string) {
	if h == nil || s == nil || channel == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.channels[channel]
	if subs == nil {
		subs = make(map[*Subscriber]struct{})
		h.channels[channel] = subs
	}
	subs[s] = struct{}{}
	h.subscribers[s] = struct{}{}
}

// Unsubscribe removes a subscriber from one channel.
func (h *Hub) Unsubscribe(s *Subscriber, channel string) {
	if h == nil || s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unsubscribeLocked(s, channel)
}

// UnsubscribeAll removes a subscriber from every channel. It is safe to call
// more than once (disconnect cleanup may race an overflow removal).
func (h *Hub) UnsubscribeAll(s *Subscriber) {
	if h == nil || s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for channel := range h.channels {
		h.unsubscribeLocked(s, channel)
	}
	delete(h.subscribers, s)
}

func (h *Hub) unsubscribeLocked(s *Subscriber, channel string) {
	subs := h.channels[channel]
	if subs == nil {
		return
	}
	delete(subs, s)
	if len(subs) == 0 {
		delete(h.channels, channel)
	}
	// The subscriber remains in h.subscribers if it still listens elsewhere.
	remaining := false
	for _, members := range h.channels {
		if _, ok := members[s]; ok {
			remaining = true
			break
		}
	}
	if !remaining {
		delete(h.subscribers, s)
	}
}

// Publish delivers a notification to every subscriber of its channel. It never
// blocks: a subscriber whose bounded queue is full is removed and marked
// overflowed so its protocol layer can disconnect it.
func (h *Hub) Publish(n Notification) {
	if h == nil || n.Channel == "" {
		return
	}
	n.LSN = h.assignLSN(n.LSN)
	h.mu.RLock()
	targets := make([]*Subscriber, 0, len(h.channels[n.Channel]))
	for s := range h.channels[n.Channel] {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	var overflowed []*Subscriber
	for _, s := range targets {
		select {
		case s.notifications <- n:
			h.published.Add(1)
		default:
			s.markOverflow()
			overflowed = append(overflowed, s)
			h.dropped.Add(1)
		}
	}

	for _, s := range overflowed {
		h.UnsubscribeAll(s)
	}
}

// assignLSN makes notification ordering monotonic within a database. A write
// transaction supplies its storage LSN; a NOTIFY-only transaction (which has no
// durable write) advances the hub's logical cursor instead of reporting zero.
func (h *Hub) assignLSN(lsn uint64) uint64 {
	for {
		current := h.lastLSN.Load()
		next := current + 1
		if lsn > next {
			next = lsn
		}
		if h.lastLSN.CompareAndSwap(current, next) {
			return next
		}
	}
}

// Stats returns the number of published and dropped notifications. It is used
// by metrics and tests.
func (h *Hub) Stats() (published, dropped uint64) {
	if h == nil {
		return 0, 0
	}
	return h.published.Load(), h.dropped.Load()
}

// Channels returns the channels with at least one subscriber.
func (h *Hub) Channels() []string {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	channels := make([]string, 0, len(h.channels))
	for channel := range h.channels {
		channels = append(channels, channel)
	}
	return channels
}
