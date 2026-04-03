package taverntest_test

import (
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestRecorder_CollectsMessages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	defer rec.Close()

	b.Publish("events", "msg1")
	b.Publish("events", "msg2")
	b.Publish("events", "msg3")

	if !rec.WaitFor(3, time.Second) {
		t.Fatal("timed out waiting for 3 messages")
	}

	rec.AssertCount(t, 3)
	rec.AssertNthMessage(t, 0, "msg1")
	rec.AssertNthMessage(t, 1, "msg2")
	rec.AssertNthMessage(t, 2, "msg3")
}

func TestRecorder_AssertContains(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	defer rec.Close()

	b.Publish("events", "hello")
	b.Publish("events", "world")

	if !rec.WaitFor(2, time.Second) {
		t.Fatal("timed out")
	}

	rec.AssertContains(t, "hello")
	rec.AssertContains(t, "world")
}

func TestRecorder_Messages_ReturnsCopy(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	defer rec.Close()

	b.Publish("events", "msg1")
	rec.WaitFor(1, time.Second)

	msgs := rec.Messages()
	if len(msgs) != 1 || msgs[0] != "msg1" {
		t.Fatalf("unexpected messages: %v", msgs)
	}

	// Mutating the returned slice should not affect the recorder
	msgs[0] = "mutated"
	rec.AssertNthMessage(t, 0, "msg1")
}

func TestRecorder_WaitFor_Timeout(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	defer rec.Close()

	// Don't publish anything — WaitFor should timeout
	ok := rec.WaitFor(1, 50*time.Millisecond)
	if ok {
		t.Fatal("expected timeout")
	}
}

func TestRecorder_Close_Idempotent(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	rec.Close()
	rec.Close() // should not panic
}

func TestScopedRecorder(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec1 := taverntest.NewScopedRecorder(b, "notif", "user-1")
	defer rec1.Close()
	rec2 := taverntest.NewScopedRecorder(b, "notif", "user-2")
	defer rec2.Close()

	b.PublishTo("notif", "user-1", "for-user-1")
	b.PublishTo("notif", "user-2", "for-user-2")

	if !rec1.WaitFor(1, time.Second) {
		t.Fatal("rec1 timed out")
	}
	if !rec2.WaitFor(1, time.Second) {
		t.Fatal("rec2 timed out")
	}

	rec1.AssertCount(t, 1)
	rec1.AssertContains(t, "for-user-1")
	rec2.AssertCount(t, 1)
	rec2.AssertContains(t, "for-user-2")
}

func TestRecorder_Len(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")
	defer rec.Close()

	if rec.Len() != 0 {
		t.Fatal("expected 0 messages initially")
	}

	b.Publish("events", "msg")
	rec.WaitFor(1, time.Second)

	if rec.Len() != 1 {
		t.Fatal("expected 1 message")
	}
}

func TestRecorder_CloseStopsCollection(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	rec := taverntest.NewRecorder(b, "events")

	b.Publish("events", "before")
	rec.WaitFor(1, time.Second)

	rec.Close()

	b.Publish("events", "after")
	time.Sleep(50 * time.Millisecond)

	rec.AssertCount(t, 1)
	rec.AssertContains(t, "before")
}
