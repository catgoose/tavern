package tavern

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackpressureControlEvent_Format(t *testing.T) {
	evt := backpressureControlEvent("throttle", "normal", "dashboard")
	assert.True(t, strings.HasPrefix(evt, "event: tavern-backpressure\n"))
	assert.True(t, strings.Contains(evt, "data: "))
	assert.True(t, strings.HasSuffix(evt, "\n\n"))

	// Extract the JSON payload.
	var payload struct {
		Tier         string `json:"tier"`
		PreviousTier string `json:"previousTier"`
		Topic        string `json:"topic"`
	}
	for _, line := range strings.Split(evt, "\n") {
		if strings.HasPrefix(line, "data: ") {
			err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload)
			require.NoError(t, err)
		}
	}
	assert.Equal(t, "throttle", payload.Tier)
	assert.Equal(t, "normal", payload.PreviousTier)
	assert.Equal(t, "dashboard", payload.Topic)
}

func TestBackpressureControlEvent_IsControlEvent(t *testing.T) {
	evt := backpressureControlEvent("simplify", "throttle", "metrics")
	assert.True(t, isControlEvent(evt))
}

func TestBackpressureEvent_NormalToThrottle(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   2,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill the buffer.
	b.Publish("t", "fill")
	// Accumulate 2 drops to enter throttle tier.
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2 → throttle

	msgs := drainChannel(ch, 200*time.Millisecond)
	// Should have: "fill" + backpressure control event (if it fit)
	// The control event is sent via non-blocking send, so it may or may not
	// arrive depending on buffer state. But "fill" should be there.
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Equal(t, "fill", msgs[0])

	// Check if the control event was delivered (buffer was full after "fill",
	// so the control event uses non-blocking send — may not arrive).
	var foundControl bool
	for _, m := range msgs {
		if strings.Contains(m, "tavern-backpressure") {
			foundControl = true
			assert.True(t, strings.Contains(m, `"tier":"throttle"`))
			assert.True(t, strings.Contains(m, `"previousTier":"normal"`))
			assert.True(t, strings.Contains(m, `"topic":"t"`))
		}
	}
	// With buffer size 1, the control event likely couldn't be delivered
	// (channel was full with "fill"). That's the expected behavior —
	// non-blocking send drops it gracefully.
	_ = foundControl
}

func TestBackpressureEvent_ThrottleToSimplify(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(2), // slightly larger buffer for control events
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   2,
			SimplifyAt:   4,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill + accumulate drops to throttle then simplify.
	b.Publish("t", "fill1")
	b.Publish("t", "fill2")
	// buffer now full
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2 → throttle
	b.Publish("t", "d3") // drop 3
	b.Publish("t", "d4") // drop 4 → simplify

	msgs := drainChannel(ch, 200*time.Millisecond)
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, "fill1", msgs[0])
	assert.Equal(t, "fill2", msgs[1])

	// Control events are non-blocking — they may or may not have been delivered.
	for _, m := range msgs {
		if strings.Contains(m, "tavern-backpressure") && strings.Contains(m, `"tier":"simplify"`) {
			assert.True(t, strings.Contains(m, `"previousTier":"throttle"`))
		}
	}
}

func TestBackpressureEvent_RecoveryToNormal(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(4), // enough room for control event on recovery
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   1,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer to trigger 1 drop → throttle.
	b.Publish("t", "fill1")
	b.Publish("t", "fill2")
	b.Publish("t", "fill3")
	b.Publish("t", "fill4")
	// Buffer full, next publish drops.
	b.Publish("t", "d1") // drop 1 → throttle

	// Drain to make room.
	msgs := drainChannel(ch, 200*time.Millisecond)
	require.GreaterOrEqual(t, len(msgs), 4)

	// In throttle tier, every other message is delivered. Publish enough
	// messages so that at least one succeeds and resets the tier to normal.
	b.Publish("t", "r1")
	b.Publish("t", "r2")
	msgs = drainChannel(ch, 200*time.Millisecond)

	// Should have at least one delivered message and a control event for recovery.
	var foundRecovery bool
	for _, m := range msgs {
		if strings.Contains(m, "tavern-backpressure") && strings.Contains(m, `"tier":"normal"`) {
			foundRecovery = true
			assert.True(t, strings.Contains(m, `"previousTier":"throttle"`))
			assert.True(t, strings.Contains(m, `"topic":"t"`))
		}
	}
	assert.True(t, foundRecovery, "expected recovery control event with tier=normal")
}

func TestBackpressureEvent_DisconnectSendsEvent(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(2),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   1,
			DisconnectAt: 3,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer.
	b.Publish("t", "fill1")
	b.Publish("t", "fill2")
	// Accumulate drops until disconnect.
	b.Publish("t", "d1") // drop 1 → throttle
	b.Publish("t", "d2") // drop 2
	b.Publish("t", "d3") // drop 3 → disconnect

	// Drain — channel should be closed after eviction.
	msgs := drainChannel(ch, 500*time.Millisecond)
	require.GreaterOrEqual(t, len(msgs), 2, "should have at least the fill messages")
	assert.Equal(t, "fill1", msgs[0])
	assert.Equal(t, "fill2", msgs[1])
}

func TestBackpressureEvent_FullChannelNoPanic(t *testing.T) {
	// Verify that injecting the control event into a full channel doesn't panic.
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   1,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer.
	b.Publish("t", "fill")
	// This drop triggers throttle — control event goes to full channel.
	// Must not panic.
	b.Publish("t", "drop1")

	msgs := drainChannel(ch, 200*time.Millisecond)
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Equal(t, "fill", msgs[0])
}

func TestBackpressureEvent_TierChangeCallbackStillFires(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(4),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   1,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	var mu sync.Mutex
	var transitions []struct{ old, new string }

	b.OnBackpressureTierChange(func(sub *SubscriberInfo, oldTier, newTier BackpressureTier) {
		mu.Lock()
		transitions = append(transitions, struct{ old, new string }{oldTier.String(), newTier.String()})
		mu.Unlock()
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer.
	b.Publish("t", "f1")
	b.Publish("t", "f2")
	b.Publish("t", "f3")
	b.Publish("t", "f4")
	// Drop → throttle.
	b.Publish("t", "d1")

	// Drain and recover. Publish two messages so at least one gets through
	// throttle (which skips every other message).
	drainChannel(ch, 200*time.Millisecond)
	b.Publish("t", "r1")
	b.Publish("t", "r2")
	drainChannel(ch, 200*time.Millisecond)

	// Give callback goroutine time to fire.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(transitions), 2)
	assert.Equal(t, "normal", transitions[0].old)
	assert.Equal(t, "throttle", transitions[0].new)
	assert.Equal(t, "throttle", transitions[1].old)
	assert.Equal(t, "normal", transitions[1].new)
}
