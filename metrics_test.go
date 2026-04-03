package tavern

import (
	"testing"
	"time"
)

func TestWithMetrics_TracksPublished(t *testing.T) {
	b := NewSSEBroker(WithMetrics())
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	for i := 0; i < 5; i++ {
		b.Publish("events", "msg")
	}

	// Drain to avoid drops.
	for i := 0; i < 5; i++ {
		<-ch
	}

	m := b.Metrics()
	if m.TopicStats["events"].Published != 5 {
		t.Fatalf("expected Published=5, got %d", m.TopicStats["events"].Published)
	}
}

func TestWithMetrics_TracksDropped(t *testing.T) {
	b := NewSSEBroker(WithMetrics(), WithBufferSize(1))
	defer b.Close()

	_, unsub := b.Subscribe("events")
	defer unsub()

	// Publish 3 without draining; buffer=1 so at least 2 should drop.
	for i := 0; i < 3; i++ {
		b.Publish("events", "msg")
	}

	m := b.Metrics()
	if m.TopicStats["events"].Dropped == 0 {
		t.Fatal("expected Dropped > 0")
	}
	total := m.TopicStats["events"].Published + m.TopicStats["events"].Dropped
	if total != 3 {
		t.Fatalf("expected Published+Dropped=3, got %d", total)
	}
}

func TestWithMetrics_PerTopic(t *testing.T) {
	b := NewSSEBroker(WithMetrics())
	defer b.Close()

	ch1, unsub1 := b.Subscribe("topic-a")
	defer unsub1()
	ch2, unsub2 := b.Subscribe("topic-b")
	defer unsub2()

	b.Publish("topic-a", "a1")
	b.Publish("topic-a", "a2")
	b.Publish("topic-b", "b1")

	<-ch1
	<-ch1
	<-ch2

	m := b.Metrics()
	if m.TopicStats["topic-a"].Published != 2 {
		t.Fatalf("expected topic-a Published=2, got %d", m.TopicStats["topic-a"].Published)
	}
	if m.TopicStats["topic-b"].Published != 1 {
		t.Fatalf("expected topic-b Published=1, got %d", m.TopicStats["topic-b"].Published)
	}
}

func TestWithMetrics_PeakSubscribers(t *testing.T) {
	b := NewSSEBroker(WithMetrics())
	defer b.Close()

	ch1, unsub1 := b.Subscribe("events")
	_, unsub2 := b.Subscribe("events")
	_, unsub3 := b.Subscribe("events")

	// Unsubscribe 2, peak should still be 3.
	unsub2()
	unsub3()

	// Publish once to create the counter entry so it shows up in Metrics.
	b.Publish("events", "msg")
	<-ch1

	m := b.Metrics()
	if m.TopicStats["events"].PeakSubscribers != 3 {
		t.Fatalf("expected PeakSubscribers=3, got %d", m.TopicStats["events"].PeakSubscribers)
	}
	unsub1()
}

func TestWithMetrics_Aggregates(t *testing.T) {
	b := NewSSEBroker(WithMetrics(), WithBufferSize(1))
	defer b.Close()

	ch1, unsub1 := b.Subscribe("x")
	defer unsub1()
	ch2, unsub2 := b.Subscribe("y")
	defer unsub2()

	b.Publish("x", "m1")
	<-ch1
	b.Publish("x", "m2")
	<-ch1
	b.Publish("y", "m3")
	<-ch2

	// Now publish to y without draining to cause a drop.
	b.Publish("y", "fill")
	b.Publish("y", "drop")

	m := b.Metrics()
	if m.TotalPublished < 3 {
		t.Fatalf("expected TotalPublished >= 3, got %d", m.TotalPublished)
	}
	if m.TotalPublished+m.TotalDropped != 5 {
		t.Fatalf("expected TotalPublished+TotalDropped=5, got %d", m.TotalPublished+m.TotalDropped)
	}
}

func TestWithMetrics_Disabled(t *testing.T) {
	b := NewSSEBroker() // no WithMetrics
	defer b.Close()

	m := b.Metrics()
	if m.TopicStats == nil {
		t.Fatal("expected non-nil TopicStats map when disabled")
	}
	if len(m.TopicStats) != 0 {
		t.Fatalf("expected empty TopicStats, got %d entries", len(m.TopicStats))
	}
}

func TestWithMetrics_PublishTo(t *testing.T) {
	b := NewSSEBroker(WithMetrics())
	defer b.Close()

	ch, unsub := b.SubscribeScoped("notif", "user-1")
	defer unsub()

	b.PublishTo("notif", "user-1", "hello")
	<-ch

	m := b.Metrics()
	if m.TopicStats["notif"].Published != 1 {
		t.Fatalf("expected Published=1 for scoped publish, got %d", m.TopicStats["notif"].Published)
	}
}

func TestWithMetrics_PublishIfChanged(t *testing.T) {
	b := NewSSEBroker(WithMetrics())
	defer b.Close()

	ch, unsub := b.Subscribe("dash")
	defer unsub()

	// First publish: should go through.
	ok := b.PublishIfChanged("dash", "v1")
	if !ok {
		t.Fatal("expected first PublishIfChanged to publish")
	}
	<-ch

	// Same content: should be skipped (NOT counted).
	ok = b.PublishIfChanged("dash", "v1")
	if ok {
		t.Fatal("expected duplicate PublishIfChanged to be skipped")
	}

	// Different content: should go through.
	ok = b.PublishIfChanged("dash", "v2")
	if !ok {
		t.Fatal("expected changed PublishIfChanged to publish")
	}
	<-ch

	m := b.Metrics()
	if m.TopicStats["dash"].Published != 2 {
		t.Fatalf("expected Published=2 for dedup publishes, got %d", m.TopicStats["dash"].Published)
	}
}

func TestWithMetrics_ZeroOverheadWhenDisabled(t *testing.T) {
	b := NewSSEBroker() // no WithMetrics
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// These should not panic.
	b.Publish("events", "msg1")
	b.PublishWithReplay("events", "msg2")
	b.PublishIfChanged("events", "msg3")
	b.PublishTo("events", "scope", "msg4")

	// Drain what arrived.
	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case <-ch:
		case <-timeout:
			return
		}
	}
}
