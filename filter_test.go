package tavern

import (
	"strings"
	"testing"
	"time"
)

func TestSubscribeWithFilter_BasicFiltering(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("news", func(msg string) bool {
		return strings.Contains(msg, "engineering")
	})
	defer unsub()

	b.Publish("news", "engineering: new release")
	b.Publish("news", "marketing: campaign launch")
	b.Publish("news", "engineering: bugfix deployed")

	// Should receive only the two engineering messages.
	got := drainN(t, msgs, 2, 500*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(got), got)
	}
	if got[0] != "engineering: new release" {
		t.Errorf("msg[0] = %q, want %q", got[0], "engineering: new release")
	}
	if got[1] != "engineering: bugfix deployed" {
		t.Errorf("msg[1] = %q, want %q", got[1], "engineering: bugfix deployed")
	}
}

func TestSubscribeWithFilter_AllMatch(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return true
	})
	defer unsub()

	b.Publish("t", "a")
	b.Publish("t", "b")

	got := drainN(t, msgs, 2, 500*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}

func TestSubscribeWithFilter_NoneMatch(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return false
	})
	defer unsub()

	b.Publish("t", "a")
	b.Publish("t", "b")

	got := drainN(t, msgs, 0, 100*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("expected 0 messages, got %d: %v", len(got), got)
	}
}

func TestSubscribeScopedWithFilter(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeScopedWithFilter("orders", "user-42", func(msg string) bool {
		return strings.Contains(msg, "shipped")
	})
	defer unsub()

	b.PublishTo("orders", "user-42", "order placed")
	b.PublishTo("orders", "user-42", "order shipped")
	b.PublishTo("orders", "user-99", "order shipped") // wrong scope
	b.PublishTo("orders", "user-42", "order delivered")

	got := drainN(t, msgs, 1, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d: %v", len(got), got)
	}
	if got[0] != "order shipped" {
		t.Errorf("msg = %q, want %q", got[0], "order shipped")
	}
}

func TestSubscribeWithFilter_ReplayFiltered(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("feed", 10)
	b.PublishWithReplay("feed", "alpha")
	b.PublishWithReplay("feed", "beta-match")
	b.PublishWithReplay("feed", "gamma")
	b.PublishWithReplay("feed", "delta-match")

	// Subscribe with filter after messages are in replay cache.
	msgs, unsub := b.SubscribeWithFilter("feed", func(msg string) bool {
		return strings.Contains(msg, "match")
	})
	defer unsub()

	got := drainN(t, msgs, 2, 500*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 replay messages, got %d: %v", len(got), got)
	}
	if got[0] != "beta-match" {
		t.Errorf("replay[0] = %q, want %q", got[0], "beta-match")
	}
	if got[1] != "delta-match" {
		t.Errorf("replay[1] = %q, want %q", got[1], "delta-match")
	}
}

func TestSubscribeWithFilter_NonMatchDoesNotCountAsDrop(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return msg == "yes"
	})
	defer unsub()

	// Publish many non-matching messages — none should count as drops.
	for i := 0; i < 100; i++ {
		b.Publish("t", "no")
	}

	drops := b.PublishDrops()
	if drops != 0 {
		t.Errorf("expected 0 drops for filtered messages, got %d", drops)
	}

	// Now publish a matching message — it should be delivered.
	b.Publish("t", "yes")
	got := drainN(t, msgs, 1, 500*time.Millisecond)
	if len(got) != 1 || got[0] != "yes" {
		t.Errorf("expected [yes], got %v", got)
	}
}

func TestSubscribeWithFilter_WithEviction(t *testing.T) {
	evicted := make(chan string, 1)
	b := NewSSEBroker(
		WithBufferSize(1),
		WithSlowSubscriberEviction(3),
		WithSlowSubscriberCallback(func(topic string) {
			evicted <- topic
		}),
	)
	defer b.Close()

	// Filtered subscriber — non-matching messages should not trigger eviction.
	_, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return msg == "match"
	})
	defer unsub()

	// Flood with non-matching messages.
	for i := 0; i < 20; i++ {
		b.Publish("t", "nope")
	}

	select {
	case <-evicted:
		t.Fatal("subscriber was evicted, but filtered messages should not count toward drops")
	case <-time.After(100 * time.Millisecond):
		// Good — no eviction.
	}
}

func TestSubscribeWithFilter_UnfilteredSubscriberUnaffected(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// One filtered, one unfiltered.
	filtered, unsub1 := b.SubscribeWithFilter("t", func(msg string) bool {
		return msg == "yes"
	})
	defer unsub1()

	unfiltered, unsub2 := b.Subscribe("t")
	defer unsub2()

	b.Publish("t", "no")
	b.Publish("t", "yes")

	// Unfiltered should get both.
	gotUnfiltered := drainN(t, unfiltered, 2, 500*time.Millisecond)
	if len(gotUnfiltered) != 2 {
		t.Fatalf("unfiltered: expected 2, got %d: %v", len(gotUnfiltered), gotUnfiltered)
	}

	// Filtered should get only "yes".
	gotFiltered := drainN(t, filtered, 1, 500*time.Millisecond)
	if len(gotFiltered) != 1 || gotFiltered[0] != "yes" {
		t.Errorf("filtered: expected [yes], got %v", gotFiltered)
	}
}

func TestSubscribeWithFilter_PublishOOB(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("oob", func(msg string) bool {
		return strings.Contains(msg, "target-id")
	})
	defer unsub()

	b.PublishOOB("oob", Replace("target-id", "<div>updated</div>"))
	b.PublishOOB("oob", Replace("other-id", "<div>skip</div>"))

	got := drainN(t, msgs, 1, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("expected 1 OOB message, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "target-id") {
		t.Errorf("expected message containing target-id, got %q", got[0])
	}
}

func TestSubscribeWithFilter_Unsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	msgs, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return true
	})

	unsub()

	// Channel should be closed after unsubscribe.
	select {
	case _, ok := <-msgs:
		if ok {
			t.Fatal("expected channel to be closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after unsubscribe")
	}
}

func TestSubscribeScopedWithFilter_ReplayFiltered(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("feed", 10)
	b.PublishWithReplay("feed", "alpha")
	b.PublishWithReplay("feed", "beta-match")

	// Scoped subscribers also receive replay from the topic-level cache.
	msgs, unsub := b.SubscribeScopedWithFilter("feed", "s1", func(msg string) bool {
		return strings.Contains(msg, "match")
	})
	defer unsub()

	got := drainN(t, msgs, 1, 500*time.Millisecond)
	if len(got) != 1 || got[0] != "beta-match" {
		t.Errorf("expected [beta-match], got %v", got)
	}
}

func TestSubscribeWithFilter_ClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	msgs, unsub := b.SubscribeWithFilter("t", func(msg string) bool {
		return true
	})
	defer unsub()

	// Channel should be immediately closed.
	select {
	case _, ok := <-msgs:
		if ok {
			t.Fatal("expected closed channel from closed broker")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed")
	}
}

func TestSubscribeScopedWithFilter_ClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	msgs, unsub := b.SubscribeScopedWithFilter("t", "s", func(msg string) bool {
		return true
	})
	defer unsub()

	select {
	case _, ok := <-msgs:
		if ok {
			t.Fatal("expected closed channel from closed broker")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed")
	}
}

// drainN reads up to n messages from ch within the timeout.
func drainN(t *testing.T, ch <-chan string, n int, timeout time.Duration) []string {
	t.Helper()
	var result []string
	deadline := time.After(timeout)
	for range n {
		select {
		case msg, ok := <-ch:
			if !ok {
				return result
			}
			result = append(result, msg)
		case <-deadline:
			return result
		}
	}
	// Brief extra drain to catch unexpected messages.
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return result
			}
			result = append(result, msg)
		case <-time.After(50 * time.Millisecond):
			return result
		}
	}
}
