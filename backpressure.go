package tavern

import (
	"sync"
	"sync/atomic"
)

// BackpressureTier represents the current backpressure tier of a subscriber.
type BackpressureTier int

const (
	// TierNormal means messages are delivered normally (0 consecutive drops).
	TierNormal BackpressureTier = iota
	// TierThrottle means the subscriber is receiving every Nth message.
	TierThrottle
	// TierSimplify means the subscriber receives simplified/lower-fidelity content.
	TierSimplify
	// TierDisconnect means the subscriber will be evicted.
	TierDisconnect
)

// String returns the tier name.
func (t BackpressureTier) String() string {
	switch t {
	case TierNormal:
		return "normal"
	case TierThrottle:
		return "throttle"
	case TierSimplify:
		return "simplify"
	case TierDisconnect:
		return "disconnect"
	default:
		return "unknown"
	}
}

// AdaptiveBackpressure configures tiered backpressure thresholds based on
// consecutive drop counts. Each threshold must be greater than the previous;
// a zero value disables that tier.
type AdaptiveBackpressure struct {
	// ThrottleAt is the consecutive drop count that triggers throttle tier.
	// In throttle tier the broker delivers every 2nd message to the subscriber.
	ThrottleAt int
	// SimplifyAt is the consecutive drop count that triggers simplify tier.
	// In simplify tier the broker attaches a fidelity hint and optionally
	// applies a simplified renderer registered for the topic.
	SimplifyAt int
	// DisconnectAt is the consecutive drop count that triggers eviction.
	DisconnectAt int
}

// adaptiveState holds per-subscriber adaptive backpressure state.
type adaptiveState struct {
	tier       atomic.Int32 // BackpressureTier stored atomically for lock-free reads
	publishSeq int64        // counts publishes seen by this subscriber for throttle modulo
}

// adaptiveBackpressure holds broker-level adaptive backpressure configuration and state.
type adaptiveBackpressure struct {
	config     AdaptiveBackpressure
	states     map[chan string]*adaptiveState
	renderers  map[string]func(string) string                               // topic → simplified renderer
	tierChange func(sub *SubscriberInfo, oldTier, newTier BackpressureTier) // optional callback
	mu         sync.RWMutex
}

// WithAdaptiveBackpressure enables tiered backpressure that adapts per-subscriber
// based on their consecutive drop count. This subsumes [WithSlowSubscriberEviction]:
// the DisconnectAt threshold acts as the eviction threshold.
//
// Tiers from lowest to highest pressure:
//   - Normal (0 drops): messages delivered normally
//   - Throttle (≥ThrottleAt drops): delivers every 2nd message
//   - Simplify (≥SimplifyAt drops): applies simplified renderer if registered
//   - Disconnect (≥DisconnectAt drops): evicts the subscriber
//
// The drop counter resets on any successful send, returning the subscriber to
// the normal tier automatically.
func WithAdaptiveBackpressure(cfg AdaptiveBackpressure) BrokerOption {
	return func(b *SSEBroker) {
		b.adaptive = &adaptiveBackpressure{
			config:    cfg,
			states:    make(map[chan string]*adaptiveState),
			renderers: make(map[string]func(string) string),
		}
		// Enable drop counting infrastructure.
		b.evictThreshold = 0 // adaptive handles eviction itself
	}
}

// SetSimplifiedRenderer registers a function that produces lightweight content
// for the given topic. When a subscriber is in the simplify tier, the renderer
// is applied to the message before delivery. If no renderer is registered,
// the original message is delivered with no transformation.
func (b *SSEBroker) SetSimplifiedRenderer(topic string, fn func(string) string) {
	if b.adaptive == nil {
		return
	}
	b.adaptive.mu.Lock()
	b.adaptive.renderers[topic] = fn
	b.adaptive.mu.Unlock()
}

// OnBackpressureTierChange sets a callback that fires whenever a subscriber
// transitions between backpressure tiers. The callback receives the subscriber
// info and the old and new tiers. The callback runs in its own goroutine.
func (b *SSEBroker) OnBackpressureTierChange(fn func(sub *SubscriberInfo, oldTier, newTier BackpressureTier)) {
	if b.adaptive == nil {
		return
	}
	b.adaptive.mu.Lock()
	b.adaptive.tierChange = fn
	b.adaptive.mu.Unlock()
}

// tierForDrops returns the backpressure tier for the given consecutive drop count.
func (ab *adaptiveBackpressure) tierForDrops(drops int64) BackpressureTier {
	if ab.config.DisconnectAt > 0 && drops >= int64(ab.config.DisconnectAt) {
		return TierDisconnect
	}
	if ab.config.SimplifyAt > 0 && drops >= int64(ab.config.SimplifyAt) {
		return TierSimplify
	}
	if ab.config.ThrottleAt > 0 && drops >= int64(ab.config.ThrottleAt) {
		return TierThrottle
	}
	return TierNormal
}

// getState returns the adaptive state for a channel, creating one if needed.
func (ab *adaptiveBackpressure) getState(ch chan string) *adaptiveState {
	ab.mu.RLock()
	st, ok := ab.states[ch]
	ab.mu.RUnlock()
	if ok {
		return st
	}
	ab.mu.Lock()
	st, ok = ab.states[ch]
	if !ok {
		st = &adaptiveState{}
		ab.states[ch] = st
	}
	ab.mu.Unlock()
	return st
}

// removeState removes the adaptive state for a channel.
func (ab *adaptiveBackpressure) removeState(ch chan string) {
	ab.mu.Lock()
	delete(ab.states, ch)
	ab.mu.Unlock()
}

// resetState resets the adaptive state tier for a channel to normal and
// returns the old tier (for tier-change callbacks).
func (ab *adaptiveBackpressure) resetState(ch chan string) (oldTier BackpressureTier, changed bool) {
	ab.mu.RLock()
	st, ok := ab.states[ch]
	ab.mu.RUnlock()
	if ok {
		old := BackpressureTier(st.tier.Load())
		if old != TierNormal {
			st.tier.Store(int32(TierNormal))
			return old, true
		}
	}
	return TierNormal, false
}

// updateTier sets the tier based on the current drop count, returning
// whether the tier changed and the old/new tiers.
func (ab *adaptiveBackpressure) updateTier(ch chan string, drops int64) (oldTier, newTier BackpressureTier, changed bool) {
	tier := ab.tierForDrops(drops)
	st := ab.getState(ch)
	old := BackpressureTier(st.tier.Load())
	if old != tier {
		st.tier.Store(int32(tier))
		return old, tier, true
	}
	return old, tier, false
}

// shouldThrottle checks whether a message should be skipped for a throttled
// subscriber. It increments the publish sequence counter and returns true
// if the message should be skipped (every other message is delivered).
func (ab *adaptiveBackpressure) shouldThrottle(ch chan string) bool {
	ab.mu.Lock()
	st, ok := ab.states[ch]
	if !ok {
		ab.mu.Unlock()
		return false
	}
	st.publishSeq++
	skip := st.publishSeq%2 != 0 // deliver even-numbered publishes
	ab.mu.Unlock()
	return skip
}

// getRenderer returns the simplified renderer for a topic, or nil.
func (ab *adaptiveBackpressure) getRenderer(topic string) func(string) string {
	ab.mu.RLock()
	fn := ab.renderers[topic]
	ab.mu.RUnlock()
	return fn
}
