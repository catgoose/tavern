package tavern

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackpressureTier_String(t *testing.T) {
	assert.Equal(t, "normal", TierNormal.String())
	assert.Equal(t, "throttle", TierThrottle.String())
	assert.Equal(t, "simplify", TierSimplify.String())
	assert.Equal(t, "disconnect", TierDisconnect.String())
	assert.Equal(t, "unknown", BackpressureTier(99).String())
}

func TestAdaptiveBackpressure_TierForDrops(t *testing.T) {
	ab := &adaptiveBackpressure{
		config: AdaptiveBackpressure{
			ThrottleAt:   5,
			SimplifyAt:   15,
			DisconnectAt: 30,
		},
	}

	assert.Equal(t, TierNormal, ab.tierForDrops(0))
	assert.Equal(t, TierNormal, ab.tierForDrops(4))
	assert.Equal(t, TierThrottle, ab.tierForDrops(5))
	assert.Equal(t, TierThrottle, ab.tierForDrops(14))
	assert.Equal(t, TierSimplify, ab.tierForDrops(15))
	assert.Equal(t, TierSimplify, ab.tierForDrops(29))
	assert.Equal(t, TierDisconnect, ab.tierForDrops(30))
	assert.Equal(t, TierDisconnect, ab.tierForDrops(100))
}

func TestAdaptiveBackpressure_ThrottleSkipsEveryOther(t *testing.T) {
	ab := &adaptiveBackpressure{
		config: AdaptiveBackpressure{ThrottleAt: 1},
		states: make(map[chan string]*adaptiveState),
	}
	ch := make(chan string, 10)
	st := &adaptiveState{}
	st.tier.Store(int32(TierThrottle))
	ab.states[ch] = st

	// seq 1 → skip, seq 2 → deliver, seq 3 → skip, seq 4 → deliver
	assert.True(t, ab.shouldThrottle(ch))
	assert.False(t, ab.shouldThrottle(ch))
	assert.True(t, ab.shouldThrottle(ch))
	assert.False(t, ab.shouldThrottle(ch))
}

func TestAdaptiveBackpressure_ThrottleTier(t *testing.T) {
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
	// Accumulate 2 drops → enters throttle tier.
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2 → throttle

	// Drain the fill message.
	drained := drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)
	assert.Equal(t, "fill", drained[0])

	// Now in throttle tier with empty buffer.
	// shouldThrottle alternates: seq 1 → skip, seq 2 → deliver.
	b.Publish("t", "throttle1") // seq 1 → skipped
	b.Publish("t", "throttle2") // seq 2 → delivered (resets counter → normal)

	drained = drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)
	assert.Equal(t, "throttle2", drained[0])
}

func TestAdaptiveBackpressure_SimplifyTier(t *testing.T) {
	// Use only SimplifyAt (no ThrottleAt) so we jump straight to simplify
	// without throttle interference.
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			SimplifyAt:   3,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	b.SetSimplifiedRenderer("t", func(msg string) string {
		return "simplified:" + msg
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer and accumulate 3 drops to reach simplify tier.
	b.Publish("t", "fill")
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2
	b.Publish("t", "d3") // drop 3 → simplify

	// Drain the fill message.
	drained := drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)
	assert.Equal(t, "fill", drained[0])

	// Now in simplify tier. Next message should be simplified.
	b.Publish("t", "data")
	drained = drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)
	assert.Equal(t, "simplified:data", drained[0])
}

func TestAdaptiveBackpressure_SimplifyTierNoRenderer(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			SimplifyAt:   2,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "fill")
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2 → simplify

	drained := drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)

	// No renderer registered — original message delivered.
	b.Publish("t", "data")
	drained = drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 1)
	assert.Equal(t, "data", drained[0])
}

func TestAdaptiveBackpressure_DisconnectTier(t *testing.T) {
	// No ThrottleAt or SimplifyAt — just disconnect at 3.
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			DisconnectAt: 3,
		}),
	)
	defer b.Close()

	ch, _ := b.Subscribe("t") // don't defer unsub — will be evicted

	// Fill buffer and accumulate drops.
	b.Publish("t", "fill")
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2
	b.Publish("t", "d3") // drop 3 → disconnect

	// Drain and wait for channel close.
	for range ch {
		// drain
	}

	assert.False(t, b.HasSubscribers("t"))
}

func TestAdaptiveBackpressure_DisconnectTierWithAllTiers(t *testing.T) {
	// With throttle and simplify configured, drops still accumulate
	// through the tiers until disconnect.
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   2,
			SimplifyAt:   4,
			DisconnectAt: 6,
		}),
	)
	defer b.Close()

	ch, _ := b.Subscribe("t") // don't defer unsub — will be evicted

	// Fill buffer, then send enough to accumulate drops through all tiers.
	b.Publish("t", "fill")
	// Need enough publishes to accumulate 6 actual drops (not throttle skips).
	// In throttle tier, some are skipped (not counted as drops).
	// In simplify tier, messages attempt send but may fail if buffer full.
	// We need to keep buffer full throughout, so never drain.
	for i := range 50 { // send many to ensure we reach disconnect
		b.Publish("t", fmt.Sprintf("msg%d", i))
	}

	// Channel should be closed (evicted).
	for range ch {
		// drain
	}

	assert.False(t, b.HasSubscribers("t"))
}

func TestAdaptiveBackpressure_RecoveryResetsToNormal(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(2),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   3,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer (size 2) and accumulate 3 drops.
	b.Publish("t", "m1")
	b.Publish("t", "m2")
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2
	b.Publish("t", "d3") // drop 3 → throttle

	// Drain everything.
	drained := drainChannel(ch, 100*time.Millisecond)
	require.GreaterOrEqual(t, len(drained), 1)

	// In throttle tier, every other publish is skipped.
	// Send two messages: first is skipped by throttle, second is delivered.
	b.Publish("t", "skip-me")  // seq 1 → skipped (throttle)
	b.Publish("t", "recovery") // seq 2 → delivered, resets counter → normal
	drained = drainChannel(ch, 100*time.Millisecond)
	require.GreaterOrEqual(t, len(drained), 1)
	assert.Equal(t, "recovery", drained[0])

	// Should be back to normal — all messages delivered.
	b.Publish("t", "normal1")
	b.Publish("t", "normal2")
	drained = drainChannel(ch, 100*time.Millisecond)
	require.Len(t, drained, 2)
	assert.Equal(t, "normal1", drained[0])
	assert.Equal(t, "normal2", drained[1])
}

func TestAdaptiveBackpressure_TierChangeCallback(t *testing.T) {
	var mu sync.Mutex
	type transition struct {
		oldTier BackpressureTier
		newTier BackpressureTier
	}
	var transitions []transition

	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   2,
			DisconnectAt: 100,
		}),
	)
	defer b.Close()

	b.OnBackpressureTierChange(func(_ *SubscriberInfo, oldTier, newTier BackpressureTier) {
		mu.Lock()
		transitions = append(transitions, transition{oldTier, newTier})
		mu.Unlock()
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill buffer and drop to enter throttle tier.
	b.Publish("t", "fill")
	b.Publish("t", "d1") // drop 1
	b.Publish("t", "d2") // drop 2 → throttle tier

	// Give callback goroutine time to fire.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	found := false
	for _, tr := range transitions {
		if tr.oldTier == TierNormal && tr.newTier == TierThrottle {
			found = true
			break
		}
	}
	mu.Unlock()
	assert.True(t, found, "expected normal→throttle transition")

	// Drain and send messages to reset to normal (throttle skips every other).
	drainChannel(ch, 100*time.Millisecond)
	b.Publish("t", "skip-me") // throttle seq 1 → skipped
	b.Publish("t", "recover") // throttle seq 2 → delivered, resets to normal
	drainChannel(ch, 100*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	foundRecovery := false
	for _, tr := range transitions {
		if tr.oldTier == TierThrottle && tr.newTier == TierNormal {
			foundRecovery = true
			break
		}
	}
	mu.Unlock()
	assert.True(t, foundRecovery, "expected throttle→normal transition on recovery")
}

func TestAdaptiveBackpressure_SetSimplifiedRendererNilAdaptive(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	// Should be a no-op, not panic.
	b.SetSimplifiedRenderer("t", func(msg string) string { return msg })
	b.OnBackpressureTierChange(func(_ *SubscriberInfo, _, _ BackpressureTier) {})
}

func TestAdaptiveBackpressure_DisconnectEvictsScoped(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			DisconnectAt: 3,
		}),
	)
	defer b.Close()

	ch, _ := b.SubscribeScoped("t", "scope1") // don't defer unsub — will be evicted

	b.PublishTo("t", "scope1", "fill")
	b.PublishTo("t", "scope1", "d1") // drop 1
	b.PublishTo("t", "scope1", "d2") // drop 2
	b.PublishTo("t", "scope1", "d3") // drop 3 → disconnect

	for range ch {
		// drain
	}

	assert.False(t, b.HasSubscribers("t"))
}

func TestAdaptiveBackpressure_OnlyThrottleConfigured(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt: 2,
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill and accumulate drops — no disconnect configured.
	b.Publish("t", "fill")
	for range 10 {
		b.Publish("t", "drop")
	}

	// Channel should still be open.
	drained := drainChannel(ch, 100*time.Millisecond)
	require.NotEmpty(t, drained)
	assert.True(t, b.HasSubscribers("t"))
}

func TestAdaptiveBackpressure_ConcurrentPublish(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(5),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   5,
			SimplifyAt:   10,
			DisconnectAt: 500, // very high to avoid eviction during race
		}),
	)
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	var wg sync.WaitGroup
	var published atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				b.Publish("t", "msg")
				published.Add(1)
			}
		}()
	}

	// Drain concurrently.
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	wg.Wait()
	assert.True(t, b.HasSubscribers("t"))
	unsub()
	<-done
}

func TestAdaptiveBackpressure_TierForDropsPartialConfig(t *testing.T) {
	// Only SimplifyAt configured — ThrottleAt and DisconnectAt are 0 (disabled).
	ab := &adaptiveBackpressure{
		config: AdaptiveBackpressure{
			SimplifyAt: 5,
		},
	}

	assert.Equal(t, TierNormal, ab.tierForDrops(0))
	assert.Equal(t, TierNormal, ab.tierForDrops(4))
	assert.Equal(t, TierSimplify, ab.tierForDrops(5))
	assert.Equal(t, TierSimplify, ab.tierForDrops(100)) // stays simplify, no disconnect
}

// drainChannel reads all available messages from a channel within the timeout.
func drainChannel(ch <-chan string, timeout time.Duration) []string {
	var msgs []string
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return msgs
			}
			msgs = append(msgs, msg)
		case <-timer.C:
			return msgs
		}
	}
}
