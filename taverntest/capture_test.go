package taverntest_test

import (
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestCapture_AssertReceived(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "alpha")
	b.Publish("events", "beta")
	b.Publish("events", "gamma")

	capt.AssertReceived(t, "alpha", "gamma") // order-independent
}

func TestCapture_AssertReceivedWithin(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "fast")

	capt.AssertReceivedWithin(t, 500*time.Millisecond, "fast")
}

func TestCapture_AssertReceivedExactly(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "first")
	b.Publish("events", "second")

	capt.AssertReceivedExactly(t, "first", "second")
}

func TestCapture_Messages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "msg1")
	b.Publish("events", "msg2")

	capt.AssertReceived(t, "msg1", "msg2")

	msgs := capt.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Mutating returned slice should not affect capture
	msgs[0] = "mutated"
	fresh := capt.Messages()
	if fresh[0] != "msg1" {
		t.Error("Messages() should return a copy")
	}
}

func TestCapture_ScopedCapture(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	cap1 := taverntest.NewScopedCapture(b, "notif", "user-1")
	defer cap1.Close()
	cap2 := taverntest.NewScopedCapture(b, "notif", "user-2")
	defer cap2.Close()

	b.PublishTo("notif", "user-1", "for-1")
	b.PublishTo("notif", "user-2", "for-2")

	cap1.AssertReceived(t, "for-1")
	cap2.AssertReceived(t, "for-2")
}

func TestCapture_Close(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")

	b.Publish("events", "before")
	capt.AssertReceived(t, "before")

	capt.Close()

	// Publishing after close should not panic or affect capture
	b.Publish("events", "after")
	time.Sleep(50 * time.Millisecond)

	msgs := capt.Messages()
	if len(msgs) != 1 {
		t.Errorf("expected 1 message after close, got %d", len(msgs))
	}
}
