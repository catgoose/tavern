package tavern

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// awaitTopicMsg reads a TopicMessage from ch within timeout, fails the test on timeout.
func awaitTopicMsg(t *testing.T, ch <-chan TopicMessage, timeout time.Duration) TopicMessage {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for TopicMessage after %v", timeout)
		return TopicMessage{} // unreachable
	}
}

// awaitStringMsg reads a string from ch within timeout, fails the test on timeout.
func awaitStringMsg(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for string message after %v", timeout)
		return "" // unreachable
	}
}

// assertNoTopicMsg asserts no TopicMessage arrives within the given duration.
func assertNoTopicMsg(t *testing.T, ch <-chan TopicMessage, dur time.Duration) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected TopicMessage: %+v", msg)
	case <-time.After(dur):
		// ok
	}
}

// assertNoStringMsg asserts no string message arrives within the given duration.
func assertNoStringMsg(t *testing.T, ch <-chan string, dur time.Duration) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected string message: %q", msg)
	case <-time.After(dur):
		// ok
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestLifeline_StaysConnectedWhileScopedStreamChurns(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control", "notifications")
	defer unsub()

	for i := 0; i < 5; i++ {
		panelCh, panelUnsub := b.SubscribeScoped("panel-data", "user:1")

		b.Publish("control", fmt.Sprintf("ctrl-%d", i))
		b.PublishTo("panel-data", "user:1", fmt.Sprintf("panel-%d", i))

		msg := awaitTopicMsg(t, ch, time.Second)
		assert.Equal(t, "control", msg.Topic)
		assert.Equal(t, fmt.Sprintf("ctrl-%d", i), msg.Data)

		pmsg := awaitStringMsg(t, panelCh, time.Second)
		assert.Equal(t, fmt.Sprintf("panel-%d", i), pmsg)

		panelUnsub()
	}

	// After all scoped churn, lifeline still works.
	b.Publish("control", "final-ctrl")
	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "control", msg.Topic)
	assert.Equal(t, "final-ctrl", msg.Data)
}

func TestLifeline_AddTopicHandoff(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	// Add dashboard topic dynamically.
	ok := b.AddTopic("sub1", "dashboard", true)
	require.True(t, ok)

	// Control event for topic addition.
	ctrl := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, topicsChangedEvent, ctrl.Topic)
	assert.Contains(t, ctrl.Data, `"action":"added"`)
	assert.Contains(t, ctrl.Data, `"topic":"dashboard"`)

	// Both topics now deliver through lifeline.
	b.Publish("control", "ctrl-msg")
	b.Publish("dashboard", "dash-msg")

	received := make(map[string]string)
	for range 2 {
		msg := awaitTopicMsg(t, ch, time.Second)
		received[msg.Topic] = msg.Data
	}
	assert.Equal(t, "ctrl-msg", received["control"])
	assert.Equal(t, "dash-msg", received["dashboard"])

	// Remove dashboard topic.
	ok = b.RemoveTopic("sub1", "dashboard", true)
	require.True(t, ok)

	// Read removal control event.
	removal := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, topicsChangedEvent, removal.Topic)
	assert.Contains(t, removal.Data, `"action":"removed"`)

	// Dashboard messages should no longer arrive.
	b.Publish("dashboard", "after-remove")
	assertNoTopicMsg(t, ch, 50*time.Millisecond)
}

func TestLifeline_ScopedReconnectDoesNotBlindApp(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control")
	defer unsub()

	panelCh, panelUnsub := b.SubscribeScoped("panel", "user:1")
	_ = panelCh
	panelUnsub() // simulate scoped stream disconnect

	b.Publish("control", "still-here")
	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "still-here", msg.Data)

	// Reconnect scoped stream.
	panelCh2, panelUnsub2 := b.SubscribeScoped("panel", "user:1")
	defer panelUnsub2()

	b.PublishTo("panel", "user:1", "panel-back")
	pmsg := awaitStringMsg(t, panelCh2, time.Second)
	assert.Equal(t, "panel-back", pmsg)
}

func TestLifeline_HandoffActivationOrdering(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	b.AddTopic("sub1", "hot-panel", false)

	// Immediately publish after AddTopic.
	b.Publish("hot-panel", "sentinel")

	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "hot-panel", msg.Topic)
	assert.Equal(t, "sentinel", msg.Data)
}

func TestLifeline_FallbackWhileScopedUnavailable(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control", "fallback-data")
	defer unsub()

	detailCh, detailUnsub := b.SubscribeScoped("detail", "user:1")
	detailUnsub() // scoped stream gone

	b.Publish("fallback-data", "fallback-signal")
	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "fallback-data", msg.Topic)

	// detail channel is closed after unsubscribe -- reads return zero value immediately.
	select {
	case v, ok := <-detailCh:
		assert.False(t, ok, "expected closed channel, got value: %q", v)
	case <-time.After(50 * time.Millisecond):
		// Also acceptable: channel already drained.
	}
}

func TestLifeline_NoDuplicateOwnership(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	b.AddTopic("sub1", "shared", false)

	standaloneCh, standaloneUnsub := b.Subscribe("shared")
	defer standaloneUnsub()

	b.Publish("shared", "one-copy")

	// Lifeline gets exactly one copy.
	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "one-copy", msg.Data)
	assertNoTopicMsg(t, ch, 50*time.Millisecond)

	// Standalone gets exactly one copy.
	smsg := awaitStringMsg(t, standaloneCh, time.Second)
	assert.Equal(t, "one-copy", smsg)
	assertNoStringMsg(t, standaloneCh, 50*time.Millisecond)

	// Remove shared from lifeline.
	b.RemoveTopic("sub1", "shared", false)

	b.Publish("shared", "after-remove")

	// Standalone still gets it.
	smsg = awaitStringMsg(t, standaloneCh, time.Second)
	assert.Equal(t, "after-remove", smsg)

	// Lifeline does NOT get it.
	assertNoTopicMsg(t, ch, 50*time.Millisecond)
}

func TestLifeline_ReplayOnPanelStreamReconnect(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("panel", 10)

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control")
	defer unsub()

	// Publish initial panel events with IDs.
	for i := 1; i <= 5; i++ {
		b.PublishWithID("panel", fmt.Sprintf("evt-%d", i), fmt.Sprintf("panel-%d", i))
	}

	// Subscribe to panel to get all replayed messages.
	panelCh, panelUnsub := b.SubscribeFromID("panel", "")
	require.NotNil(t, panelCh)

	// Drain all 5 replay messages plus the reconnected control event.
	var lastID string
	for i := 0; i < 5; i++ {
		msg := awaitStringMsg(t, panelCh, time.Second)
		// Messages have "id: evt-N\n..." format injected.
		for _, line := range strings.Split(msg, "\n") {
			if strings.HasPrefix(line, "id: ") {
				lastID = strings.TrimPrefix(line, "id: ")
			}
		}
	}
	assert.Equal(t, "evt-5", lastID)
	panelUnsub()

	// Publish more events.
	for i := 6; i <= 8; i++ {
		b.PublishWithID("panel", fmt.Sprintf("evt-%d", i), fmt.Sprintf("panel-%d", i))
	}

	// Reconnect from last known ID.
	panelCh2, panelUnsub2 := b.SubscribeFromID("panel", "evt-5")
	require.NotNil(t, panelCh2)
	defer panelUnsub2()

	// First message is the tavern-reconnected control event -- drain it.
	reconnectMsg := awaitStringMsg(t, panelCh2, time.Second)
	assert.Contains(t, reconnectMsg, "tavern-reconnected")

	// Assert next 3 messages are the replayed ones.
	for i := 6; i <= 8; i++ {
		msg := awaitStringMsg(t, panelCh2, time.Second)
		assert.Contains(t, msg, fmt.Sprintf("panel-%d", i))
	}

	// Meanwhile, lifeline is never interrupted.
	b.Publish("control", "control-during-replay")
	lmsg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "control-during-replay", lmsg.Data)
}

func TestLifeline_ScopedIsolation(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control")
	defer unsub()

	scopedCh, scopedUnsub := b.SubscribeScoped("panel", "user:1")
	defer scopedUnsub()

	b.PublishTo("panel", "user:1", "scoped-msg")
	smsg := awaitStringMsg(t, scopedCh, time.Second)
	assert.Equal(t, "scoped-msg", smsg)

	// Lifeline does NOT get panel messages.
	assertNoTopicMsg(t, ch, 50*time.Millisecond)

	b.Publish("control", "ctrl-msg")
	lmsg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "ctrl-msg", lmsg.Data)

	// Scoped does NOT get control messages.
	assertNoStringMsg(t, scopedCh, 50*time.Millisecond)
}

func TestLifeline_ConcurrentHandoff(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "base")
	defer unsub()

	var wg sync.WaitGroup

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			topic := fmt.Sprintf("t-%d", id)
			for j := 0; j < 20; j++ {
				b.AddTopic("sub1", topic, false)
				b.Publish("base", fmt.Sprintf("ping-%d-%d", id, j))
				b.RemoveTopic("sub1", topic, false)
			}
		}(g)
	}

	// Drain lifeline messages in the background so channels don't block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	wg.Wait()
	// Test completing without panic is the assertion.
	<-done
}

func TestLifeline_ScopedReconnectCycleDoesNotLeakToLifeline(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app"}, "control", "notifications")
	defer unsub()

	// Simulate multiple scoped stream connect/disconnect cycles.
	for i := 0; i < 10; i++ {
		scopedCh, scopedUnsub := b.SubscribeScoped("dashboard", "user:1")
		b.PublishTo("dashboard", "user:1", fmt.Sprintf("data-%d", i))
		msg := awaitStringMsg(t, scopedCh, time.Second)
		assert.Equal(t, fmt.Sprintf("data-%d", i), msg)
		scopedUnsub()
	}

	// Lifeline is untouched by scoped churn.
	b.Publish("control", "after-churn")
	msg := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, "control", msg.Topic)
	assert.Equal(t, "after-churn", msg.Data)

	// Verify no stale messages leaked onto the lifeline.
	assertNoTopicMsg(t, ch, 100*time.Millisecond)
}

func TestLifeline_TopicSetQueryDuringHandoff(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	topics := b.SubscriberTopics("sub1")
	require.Len(t, topics, 1)
	assert.Contains(t, topics, "control")

	b.AddTopic("sub1", "panel-a", false)
	b.AddTopic("sub1", "panel-b", false)

	topics = b.SubscriberTopics("sub1")
	assert.Len(t, topics, 3)
	assert.ElementsMatch(t, []string{"control", "panel-a", "panel-b"}, topics)

	b.RemoveTopic("sub1", "panel-a", false)
	topics = b.SubscriberTopics("sub1")
	assert.Len(t, topics, 2)
	assert.ElementsMatch(t, []string{"control", "panel-b"}, topics)
}

func TestLifeline_AddRemoveWithControlEvents_TopicListAccuracy(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	// Add three topics with control events.
	for _, topic := range []string{"panel-a", "panel-b", "panel-c"} {
		b.AddTopic("sub1", topic, true)
		ctrl := awaitTopicMsg(t, ch, time.Second)
		assert.Equal(t, topicsChangedEvent, ctrl.Topic)
		assert.Contains(t, ctrl.Data, `"action":"added"`)
	}

	// Remove middle topic.
	b.RemoveTopic("sub1", "panel-b", true)
	ctrl := awaitTopicMsg(t, ch, time.Second)
	assert.Equal(t, topicsChangedEvent, ctrl.Topic)
	assert.Contains(t, ctrl.Data, `"action":"removed"`)
	assert.Contains(t, ctrl.Data, `"topic":"panel-b"`)

	// Verify SubscriberTopics matches what the control events reported.
	topics := b.SubscriberTopics("sub1")
	assert.ElementsMatch(t, []string{"control", "panel-a", "panel-c"}, topics)
}

func TestLifeline_MultipleLifelinesIndependent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app1"}, "control")
	defer unsub1()

	ch2, unsub2 := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "app2"}, "control")
	defer unsub2()

	// Add different topics to each lifeline.
	b.AddTopic("app1", "panel-a", false)
	b.AddTopic("app2", "panel-b", false)

	b.Publish("panel-a", "for-app1")
	b.Publish("panel-b", "for-app2")

	// app1 gets panel-a but not panel-b.
	msg1 := awaitTopicMsg(t, ch1, time.Second)
	assert.Equal(t, "panel-a", msg1.Topic)
	assert.Equal(t, "for-app1", msg1.Data)

	// app2 gets panel-b but not panel-a.
	msg2 := awaitTopicMsg(t, ch2, time.Second)
	assert.Equal(t, "panel-b", msg2.Topic)
	assert.Equal(t, "for-app2", msg2.Data)

	// No cross-contamination.
	assertNoTopicMsg(t, ch1, 50*time.Millisecond)
	assertNoTopicMsg(t, ch2, 50*time.Millisecond)
}
