package tavern

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeFromIDWith_FilterAppliesToReplayAndLive(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(32))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 5; i++ {
		var body string
		if i%2 == 0 {
			body = fmt.Sprintf("keep-%d", i)
		} else {
			body = fmt.Sprintf("drop-%d", i)
		}
		b.PublishWithID("events", fmt.Sprintf("%d", i), body)
	}

	ch, unsub := b.SubscribeFromIDWith("events", "1",
		SubWithFilter(func(msg string) bool {
			return strings.Contains(msg, "keep")
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	// Replay messages 2..5, but only "keep-2" and "keep-4" should pass.
	var replay []string
	timeout := time.After(200 * time.Millisecond)
	for len(replay) < 2 {
		select {
		case msg := <-ch:
			replay = append(replay, msg)
		case <-timeout:
			t.Fatalf("timed out waiting for filtered replay messages, got %v", replay)
		}
	}

	// Reconnected control event comes after replay (always bypasses filter).
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}
	require.Len(t, replay, 2)
	assert.Contains(t, replay[0], "keep-2")
	assert.Contains(t, replay[1], "keep-4")

	// Verify the filtered-out messages are not buffered.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra replay message: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// Now publish more live messages and verify filter still applies.
	b.PublishWithID("events", "6", "drop-6")
	b.PublishWithID("events", "7", "keep-7")
	b.PublishWithID("events", "8", "drop-8")
	b.PublishWithID("events", "9", "keep-9")

	var live []string
	timeout = time.After(time.Second)
	for len(live) < 2 {
		select {
		case msg := <-ch:
			live = append(live, msg)
		case <-timeout:
			t.Fatalf("timed out waiting for live messages, got %v", live)
		}
	}
	require.Len(t, live, 2)
	assert.Contains(t, live[0], "keep-7")
	assert.Contains(t, live[1], "keep-9")
}

func TestSubscribeFromIDWith_MetaAppliesOnResume(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	_, unsub := b.SubscribeFromIDWith("events", "1",
		SubWithMeta(SubscribeMeta{
			ID:   "user-1",
			Meta: map[string]string{"role": "admin"},
		}),
	)
	defer unsub()

	subs := b.Subscribers("events")
	require.Len(t, subs, 1)
	assert.Equal(t, "user-1", subs[0].ID)
	assert.Equal(t, "events", subs[0].Topic)
	require.NotNil(t, subs[0].Meta)
	assert.Equal(t, "admin", subs[0].Meta["role"])
}

func TestSubscribeFromIDWith_RateLimitsLiveNotReplay(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(64))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 5; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("replay-%d", i))
	}

	ch, unsub := b.SubscribeFromIDWith("events", "",
		SubWithRate(Rate{MinInterval: 200 * time.Millisecond}),
	)
	require.NotNil(t, ch)
	defer unsub()

	// All 5 replay messages should arrive in a burst (well under the rate).
	start := time.Now()
	var replay []string
	for len(replay) < 5 {
		select {
		case msg := <-ch:
			replay = append(replay, msg)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for replay burst, got %d", len(replay))
		}
	}
	burstElapsed := time.Since(start)
	assert.Less(t, burstElapsed, 100*time.Millisecond, "replay should not be rate-limited")
	require.Len(t, replay, 5)

	// Now publish two live messages back-to-back. The first should arrive
	// quickly (no held state) and the second should be held by the rate
	// limiter and delivered after the interval as the latest-wins value.
	b.PublishWithID("events", "10", "live-a")
	// Drain the first live message.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "live-a")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first live message")
	}

	// Publish two more in quick succession; only the latest should arrive
	// after the rate-limit interval, not immediately.
	b.PublishWithID("events", "11", "live-b")
	b.PublishWithID("events", "12", "live-c")

	// Within ~50ms (well under 200ms interval) we should NOT see live-c yet.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected immediate delivery while rate-limited: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// After the interval elapses, live-c should be delivered (latest-wins).
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "live-c")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for held live message")
	}
}

func TestSubscribeFromIDWith_SnapshotOnFreshSubscribe(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	ch, unsub := b.SubscribeFromIDWith("events", "",
		SubWithSnapshot(func() string { return "snapshot-payload" }),
	)
	require.NotNil(t, ch)
	defer unsub()

	// Snapshot must be the first message delivered.
	select {
	case msg := <-ch:
		assert.Equal(t, "snapshot-payload", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for snapshot on fresh subscribe")
	}

	// Live messages still flow.
	b.Publish("events", "live")
	select {
	case msg := <-ch:
		assert.Equal(t, "live", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSubscribeFromIDWith_SnapshotSkippedOnResume(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	snapCalled := false
	ch, unsub := b.SubscribeFromIDWith("events", "1",
		SubWithSnapshot(func() string {
			snapCalled = true
			return "should-not-be-delivered"
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	assert.False(t, snapCalled, "snapshot fn must not be invoked on resume")

	// Replay messages 2 and 3 come first.
	var replay []string
	for len(replay) < 2 {
		select {
		case msg := <-ch:
			replay = append(replay, msg)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for replay, got %d", len(replay))
		}
	}
	require.Len(t, replay, 2)
	assert.Contains(t, replay[0], "msg-2")
	assert.Contains(t, replay[1], "msg-3")

	// Reconnected control event comes after replay (not the snapshot).
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
		assert.NotContains(t, msg, "should-not-be-delivered")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// And nothing else.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message after replay: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeFromIDWith_ReconnectControlEventBypassesFilter(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "drop-1")
	b.PublishWithID("events", "2", "drop-2")

	// Filter rejects everything to prove the control event bypasses it.
	ch, unsub := b.SubscribeFromIDWith("events", "1",
		SubWithFilter(func(msg string) bool { return false }),
	)
	require.NotNil(t, ch)
	defer unsub()

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected",
			"reconnect control event must be delivered even when the filter rejects everything")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// No filtered replay should arrive.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message past the reject-all filter: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// Live publishes should also be filtered out.
	b.PublishWithID("events", "3", "drop-3")
	select {
	case msg := <-ch:
		t.Fatalf("unexpected live message past the reject-all filter: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeFromIDWith_GapFallbackStillWorks(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, func() string {
		return "gap-fallback-snapshot"
	})

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	// Subscribe with an unknown ID AND a user snapshot. The user snapshot
	// should NOT fire (resume path), but the gap-fallback snapshot SHOULD.
	userSnapCalled := false
	ch, unsub := b.SubscribeFromIDWith("events", "unknown-id",
		SubWithSnapshot(func() string {
			userSnapCalled = true
			return "user-snapshot-should-not-fire"
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	assert.False(t, userSnapCalled, "user snapshot must not fire on resume")

	// First: gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// Second: gap-fallback snapshot.
	select {
	case msg := <-ch:
		assert.Equal(t, "gap-fallback-snapshot", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap-fallback snapshot")
	}

	// Third: reconnected control event with replay stats.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}
}

func TestSubscribeFromIDWith_FreshSubscribeNoReconnectEvent(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	ch, unsub := b.SubscribeFromIDWith("events", "")
	require.NotNil(t, ch)
	defer unsub()

	// No control event should arrive: this is a fresh subscribe.
	b.Publish("events", "first-live")
	select {
	case msg := <-ch:
		assert.Equal(t, "first-live", msg, "fresh subscribe must not emit a reconnect control event")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSubscribeFromIDWith_UnsubscribeCleansFilterAndMeta(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	ch, unsub := b.SubscribeFromIDWith("events", "1",
		SubWithFilter(func(msg string) bool { return true }),
		SubWithMeta(SubscribeMeta{ID: "user-9"}),
	)
	require.NotNil(t, ch)

	require.Len(t, b.Subscribers("events"), 1)
	unsub()

	// After unsubscribe the broker should report no subscribers and the
	// filterPredicates / subscriberMeta entries should be gone.
	assert.Empty(t, b.Subscribers("events"))

	b.mu.RLock()
	filterCount := len(b.filterPredicates)
	metaCount := len(b.subscriberMeta)
	b.mu.RUnlock()
	assert.Zero(t, filterCount, "filterPredicates should be cleaned up on unsubscribe")
	assert.Zero(t, metaCount, "subscriberMeta should be cleaned up on unsubscribe")
}
