package storage

import (
	"testing"
	"time"
)

func TestHubSubscribePublishUnsubscribe(t *testing.T) {
	h := NewHub()
	a := NewSubscriber(1, 4)
	b := NewSubscriber(2, 4)
	h.Subscribe(a, "orders")
	h.Subscribe(a, "orders") // idempotent
	h.Subscribe(b, "other")

	h.Publish(Notification{PID: 9, Channel: "orders", Payload: "one"})
	select {
	case n := <-a.Notifications():
		if n.Payload != "one" || n.LSN == 0 {
			t.Fatalf("unexpected notification: %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
	select {
	case n := <-b.Notifications():
		t.Fatalf("received notification for another channel: %+v", n)
	default:
	}

	h.Unsubscribe(a, "orders")
	h.Publish(Notification{Channel: "orders", Payload: "two"})
	select {
	case n := <-a.Notifications():
		t.Fatalf("received notification after unsubscribe: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubSlowConsumerIsDropped(t *testing.T) {
	h := NewHub()
	s := NewSubscriber(1, 2)
	h.Subscribe(s, "ch")

	for i := 0; i < 5; i++ {
		h.Publish(Notification{Channel: "ch", Payload: "x"})
	}
	select {
	case <-s.Overflow():
	case <-time.After(time.Second):
		t.Fatal("overflow signal not closed")
	}
	if channels := h.Channels(); len(channels) != 0 {
		t.Fatalf("slow consumer still subscribed: %v", channels)
	}
	published, dropped := h.Stats()
	if published != 2 || dropped == 0 {
		t.Fatalf("published=%d dropped=%d", published, dropped)
	}
}

func TestHubsAreIsolated(t *testing.T) {
	one := NewHub()
	two := NewHub()
	s := NewSubscriber(1, 2)
	one.Subscribe(s, "ch")
	two.Publish(Notification{Channel: "ch", Payload: "nope"})
	select {
	case n := <-s.Notifications():
		t.Fatalf("cross-hub notification: %+v", n)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotificationLSNMonotonicAcrossNotifyOnlyPublishes(t *testing.T) {
	h := NewHub()
	s := NewSubscriber(1, 8)
	h.Subscribe(s, "ch")
	h.Publish(Notification{Channel: "ch", Payload: "a"})
	h.Publish(Notification{Channel: "ch", Payload: "b", LSN: 1000})
	h.Publish(Notification{Channel: "ch", Payload: "c"})

	var got []uint64
	for i := 0; i < 3; i++ {
		got = append(got, (<-s.Notifications()).LSN)
	}
	if got[0] == 0 || got[1] != 1000 || got[2] <= got[1] {
		t.Fatalf("lsns = %v", got)
	}
}
