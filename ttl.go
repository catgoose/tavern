package tavern

import (
	"context"
	"time"
)

// TTLOption configures optional behavior for TTL-based publish methods such
// as [SSEBroker.PublishWithTTL] and [SSEBroker.PublishToWithTTL].
type TTLOption func(*ttlConfig)

type ttlConfig struct {
	autoRemoveID string // element ID to send OOB delete on expiry
}

// WithAutoRemove configures a TTL publish to automatically send an OOB delete
// fragment for the given element ID when the message expires from the replay
// cache. This removes the element from currently-connected clients' DOM.
func WithAutoRemove(elementID string) TTLOption {
	return func(c *ttlConfig) {
		c.autoRemoveID = elementID
	}
}

// PublishWithTTL behaves like [SSEBroker.PublishWithReplay] but marks the
// replay cache entry with a time-to-live. After the TTL expires, the entry is
// removed from the replay cache so new subscribers do not see stale messages.
// The message is delivered immediately to all current subscribers regardless of
// the TTL.
func (b *SSEBroker) PublishWithTTL(topic, msg string, ttl time.Duration, opts ...TTLOption) {
	cfg := parseTTLOpts(opts)
	now := time.Now()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	entry := b.storeReplayWithTTL(topic, msg, now, ttl, cfg)
	store := b.replayStore

	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	if store != nil {
		_ = store.Append(context.Background(), topic, entry)
	}

	b.ensureTTLSweeper()

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
}

// PublishOOBWithTTL renders the given fragments and publishes them with a TTL
// on the replay cache entry. See [SSEBroker.PublishWithTTL] for TTL semantics.
func (b *SSEBroker) PublishOOBWithTTL(topic string, ttl time.Duration, fragments ...Fragment) {
	b.PublishWithTTL(topic, RenderFragments(fragments...), ttl)
}

// PublishToWithTTL publishes msg to scoped subscribers with a TTL on the
// replay cache entry. See [SSEBroker.PublishWithTTL] for TTL semantics.
func (b *SSEBroker) PublishToWithTTL(topic, scope, msg string, ttl time.Duration, opts ...TTLOption) {
	cfg := parseTTLOpts(opts)
	now := time.Now()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	entry := b.storeReplayWithTTL(topic, msg, now, ttl, cfg)
	store := b.replayStore

	scopedSubs, exists := b.scopedTopics[topic]
	var channels []chan string
	if exists {
		channels = make([]chan string, 0, len(scopedSubs))
		for ch, sub := range scopedSubs {
			if sub.scope == scope {
				channels = append(channels, ch)
			}
		}
	}
	b.mu.Unlock()

	if store != nil {
		_ = store.Append(context.Background(), topic, entry)
	}

	b.ensureTTLSweeper()

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
}

// PublishIfChangedWithTTL combines deduplication with TTL. The message is
// published only if it differs from the last PublishIfChanged value for the
// topic, and the replay cache entry expires after the given TTL. Returns true
// if published (content changed), false if skipped.
func (b *SSEBroker) PublishIfChangedWithTTL(topic, msg string, ttl time.Duration, opts ...TTLOption) bool {
	cfg := parseTTLOpts(opts)
	h := hashMsg(msg)
	now := time.Now()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	if prev, ok := b.lastHash[topic]; ok && prev == h {
		b.mu.Unlock()
		return false
	}
	b.lastHash[topic] = h
	entry := b.storeReplayWithTTL(topic, msg, now, ttl, cfg)
	store := b.replayStore

	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	if store != nil {
		_ = store.Append(context.Background(), topic, entry)
	}

	b.ensureTTLSweeper()

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
	return true
}

// storeReplayWithTTL adds a message to both the replay cache and replay log
// with TTL metadata. Must be called with b.mu held. When an external
// ReplayStore is configured, this only builds and returns the entry; the
// caller is responsible for appending it to the store after releasing the lock.
func (b *SSEBroker) storeReplayWithTTL(topic, msg string, now time.Time, ttl time.Duration, cfg ttlConfig) ReplayEntry {
	entry := ReplayEntry{
		Msg:         msg,
		ExpiresAt:   now.Add(ttl),
		PublishedAt: now,
	}
	if cfg.autoRemoveID != "" {
		entry.AutoRemoveID = cfg.autoRemoveID
	}

	if b.replayStore != nil {
		return entry
	}

	maxSize := 1
	if n, ok := b.replaySize[topic]; ok {
		maxSize = n
	}

	// Update replay cache.
	msgs := b.replayCache[topic]
	msgs = append(msgs, msg)
	if len(msgs) > maxSize {
		msgs = msgs[len(msgs)-maxSize:]
	}
	b.replayCache[topic] = msgs

	// Update replay log with TTL metadata.
	log := b.replayLog[topic]
	log = append(log, entry)
	if len(log) > maxSize {
		log = log[len(log)-maxSize:]
	}
	b.replayLog[topic] = log
	return entry
}

// ensureTTLSweeper starts the background TTL sweeper goroutine if it has not
// been started yet. It is safe to call from multiple goroutines.
func (b *SSEBroker) ensureTTLSweeper() {
	b.ttlSweeperOnce.Do(func() {
		go b.ttlSweepLoop()
	})
}

// ttlSweepLoop periodically removes expired entries from the replay cache and
// replay log. When an expired entry has an AutoRemoveID, an OOB delete
// fragment is published to connected subscribers.
func (b *SSEBroker) ttlSweepLoop() {
	ticker := time.NewTicker(b.ttlSweepInterval())
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case now := <-ticker.C:
			b.sweepExpiredEntries(now)
		}
	}
}

// ttlSweepInterval returns the sweep interval. Defaults to 1 second.
func (b *SSEBroker) ttlSweepInterval() time.Duration {
	if b.msgTTLSweep > 0 {
		return b.msgTTLSweep
	}
	return time.Second
}

// sweepExpiredEntries removes expired TTL entries and publishes auto-remove
// fragments for entries that have an AutoRemoveID.
func (b *SSEBroker) sweepExpiredEntries(now time.Time) {
	type autoRemoval struct {
		topic     string
		elementID string
	}
	var removals []autoRemoval

	b.mu.Lock()
	for topic, log := range b.replayLog {
		var kept []ReplayEntry
		var keptMsgs []string
		cache := b.replayCache[topic]

		// Build a set of messages to remove based on expired log entries.
		expiredMsgs := make(map[string]struct{})
		for _, entry := range log {
			if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
				expiredMsgs[entry.Msg] = struct{}{}
				if entry.AutoRemoveID != "" {
					removals = append(removals, autoRemoval{
						topic:     topic,
						elementID: entry.AutoRemoveID,
					})
				}
			} else {
				kept = append(kept, entry)
			}
		}

		if len(expiredMsgs) > 0 {
			// Filter replay cache to remove expired messages.
			for _, msg := range cache {
				if _, expired := expiredMsgs[msg]; !expired {
					keptMsgs = append(keptMsgs, msg)
				}
			}
			if len(keptMsgs) == 0 {
				delete(b.replayCache, topic)
			} else {
				b.replayCache[topic] = keptMsgs
			}
		}

		if len(kept) == 0 {
			delete(b.replayLog, topic)
		} else {
			b.replayLog[topic] = kept
		}
	}
	b.mu.Unlock()

	// Publish auto-remove fragments outside the lock.
	for _, r := range removals {
		b.PublishOOB(r.topic, Delete(r.elementID))
	}
}

func parseTTLOpts(opts []TTLOption) ttlConfig {
	var cfg ttlConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
