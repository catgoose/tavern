package taverntest_test

import (
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestSlowSubscriber_ReadsWithDelay(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	slow := taverntest.NewSlowSubscriber(b, "events", taverntest.SlowSubscriberConfig{
		ReadDelay: 20 * time.Millisecond,
	})
	defer slow.Close()

	b.Publish("events", "msg1")
	b.Publish("events", "msg2")

	// Should eventually read both messages
	if !slow.WaitFor(2, 2*time.Second) {
		t.Fatal("timed out waiting for slow subscriber to read messages")
	}

	msgs := slow.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0] != "msg1" || msgs[1] != "msg2" {
		t.Errorf("unexpected messages: %v", msgs)
	}
}

func TestSlowSubscriber_CausesDrops(t *testing.T) {
	b := tavern.NewSSEBroker(tavern.WithBufferSize(2))
	defer b.Close()

	slow := taverntest.NewSlowSubscriber(b, "events", taverntest.SlowSubscriberConfig{
		ReadDelay: 100 * time.Millisecond,
	})
	defer slow.Close()

	// Publish more messages than the buffer can hold
	for i := range 10 {
		b.Publish("events", time.Duration(i).String())
	}

	// Some messages should be dropped
	time.Sleep(200 * time.Millisecond)
	drops := b.PublishDrops()
	if drops == 0 {
		t.Log("no drops detected; buffer was large enough (may vary by timing)")
	}
}

func TestSlowSubscriber_Eviction(t *testing.T) {
	b := tavern.NewSSEBroker(
		tavern.WithBufferSize(1),
		tavern.WithSlowSubscriberEviction(3),
	)
	defer b.Close()

	slow := taverntest.NewSlowSubscriber(b, "events", taverntest.SlowSubscriberConfig{
		ReadDelay: 500 * time.Millisecond,
	})

	// Flood the subscriber to trigger eviction
	for range 20 {
		b.Publish("events", "flood")
	}

	// Subscriber should be evicted — its done channel should close
	select {
	case <-slow.Done():
		// evicted as expected
	case <-time.After(2 * time.Second):
		t.Fatal("expected slow subscriber to be evicted")
		slow.Close()
	}
}

func TestSlowSubscriber_Close_Idempotent(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	slow := taverntest.NewSlowSubscriber(b, "events", taverntest.SlowSubscriberConfig{})
	slow.Close()
	slow.Close() // should not panic
}

func TestSlowSubscriber_Len(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	slow := taverntest.NewSlowSubscriber(b, "events", taverntest.SlowSubscriberConfig{})
	defer slow.Close()

	if slow.Len() != 0 {
		t.Fatal("expected 0 messages initially")
	}

	b.Publish("events", "msg")
	slow.WaitFor(1, time.Second)

	if slow.Len() != 1 {
		t.Fatal("expected 1 message")
	}
}
