package tavern

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeFromID_GapSilentDefault(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 3)
	// Publish 5 messages so IDs 1-2 roll out of the log.
	for i := 1; i <= 5; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Subscribe with a rolled-out ID. Default strategy is GapSilent.
	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Reconnected control event is always sent.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// No replay or gap event should arrive with GapSilent.
	select {
	case msg := <-ch:
		t.Fatalf("expected no message with GapSilent default, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// Live messages still work.
	b.PublishWithID("events", "6", "live")
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "live")
		assert.Contains(t, msg, "id: 6")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSubscribeFromID_GapControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")
	// ID "1" is now evicted (policy size 2, log has IDs 2,3).

	// Set a non-silent gap strategy so the control event is sent.
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string { return "" })

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First: gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
		assert.Contains(t, msg, `"lastEventId":"1"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// Then: reconnected control event with replay stats.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}
}

func TestSubscribeFromID_GapFallbackToSnapshot(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	snapshotData := "<div>full state</div>"
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string {
		return snapshotData
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First: gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// Second: snapshot.
	select {
	case msg := <-ch:
		assert.Equal(t, snapshotData, msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for snapshot")
	}

	// Third: reconnected control event with replay stats.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// No more buffered messages (no replay since ID was not found).
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeFromID_GapSnapshotEmpty(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string {
		return "" // empty snapshot
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First: gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// Then: reconnected control event with replay stats.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	select {
	case msg := <-ch:
		t.Fatalf("expected no snapshot for empty return, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOnReplayGap_CallbackFires(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	var mu sync.Mutex
	var capturedSub *SubscriberInfo
	var capturedID string

	b.OnReplayGap("events", func(sub *SubscriberInfo, lastEventID string) {
		mu.Lock()
		capturedSub = sub
		capturedID = lastEventID
		mu.Unlock()
	})

	// Callbacks fire even with default GapSilent strategy.
	_, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Give callback goroutine time to fire.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, capturedSub)
	assert.Equal(t, "events", capturedSub.Topic)
	assert.Equal(t, "1", capturedID)
}

func TestOnReplayGap_MultipleCallbacks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	var mu sync.Mutex
	var count int

	b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	_, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, 2, count, "both callbacks should fire")
	mu.Unlock()
}

func TestOnReplayGap_NoGapNoCallback(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	var fired bool
	b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {
		fired = true
	})

	// ID "1" is in the log, so no gap.
	_, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	time.Sleep(50 * time.Millisecond)
	assert.False(t, fired, "callback should not fire when ID is found")
}

func TestOnReplayGap_EmptyLastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	var fired bool
	b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {
		fired = true
	})

	// Empty Last-Event-ID means "replay all", not a gap.
	_, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	time.Sleep(50 * time.Millisecond)
	assert.False(t, fired, "callback should not fire for empty Last-Event-ID")
}

func TestOnReplayGap_ClosedBrokerNoop(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	require.NotPanics(t, func() {
		b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {})
	})
}

func TestSetReplayGapPolicy_ClosedBrokerNoop(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	require.NotPanics(t, func() {
		b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string { return "snap" })
	})
}

func TestGapControlEventFormat(t *testing.T) {
	msg := replayGapControlEvent("abc-123")
	assert.True(t, strings.HasPrefix(msg, "event: tavern-replay-gap\n"))
	assert.Contains(t, msg, `"lastEventId":"abc-123"`)
	assert.True(t, strings.HasSuffix(msg, "\n\n"), "SSE messages must end with double newline")
}

func TestSubscribeFromID_GapWithLiveMessages(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string {
		return "<snapshot>"
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Drain reconnected control event, gap control event, and snapshot.
	<-ch
	<-ch
	<-ch

	// Live messages should still arrive.
	b.PublishWithID("events", "4", "live")
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "live")
		assert.Contains(t, msg, "id: 4")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSetReplayGapPolicy_ClearedBySetReplayPolicy(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 5)
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string { return "snap" })

	// Disabling replay should also clear gap policy.
	b.SetReplayPolicy("events", 0)

	b.mu.RLock()
	_, hasStrategy := b.replayGapStrategy["events"]
	_, hasSnapshot := b.replayGapSnapshot["events"]
	b.mu.RUnlock()

	assert.False(t, hasStrategy)
	assert.False(t, hasSnapshot)
}

func TestSetReplayGapPolicy_ClearedByClearReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 5)
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string { return "snap" })

	b.ClearReplay("events")

	b.mu.RLock()
	_, hasStrategy := b.replayGapStrategy["events"]
	_, hasSnapshot := b.replayGapSnapshot["events"]
	b.mu.RUnlock()

	assert.False(t, hasStrategy)
	assert.False(t, hasSnapshot)
}

func TestSetReplayGapPolicy_NilSnapshotFn(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	// First set with a snapshot function.
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string { return "snap" })
	// Then set with nil to clear the snapshot function.
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, nil)

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First: gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// Then: reconnected control event with replay stats.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	select {
	case msg := <-ch:
		t.Fatalf("expected no snapshot when snapshotFn is nil, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeFromID_GapSilentNoControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	// Explicitly set GapSilent — should behave same as default.
	b.SetReplayGapPolicy("events", GapSilent, nil)

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Reconnected control event is always sent.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// GapSilent should not send a gap control event.
	select {
	case msg := <-ch:
		t.Fatalf("expected no gap control event with GapSilent, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSetReplayGapPolicy_WarnsWithoutReplayState(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := NewSSEBroker(WithLogger(logger))
	defer b.Close()

	b.SetReplayGapPolicy("feed", GapFallbackToSnapshot, func() string {
		return "<div>snapshot</div>"
	})

	assert.Contains(t, buf.String(), "no ID-backed replay state")
}

func TestSetReplayGapPolicy_NoWarnWithReplayState(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := NewSSEBroker(WithLogger(logger))
	defer b.Close()

	b.SetReplayPolicy("feed", 10)
	b.PublishWithID("feed", "evt-1", "data: hello\n\n")
	b.SetReplayGapPolicy("feed", GapFallbackToSnapshot, func() string {
		return "<div>snapshot</div>"
	})

	assert.NotContains(t, buf.String(), "no ID-backed replay state")
}
