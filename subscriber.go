package tavern

import (
	"encoding/json"
	"time"
)

// SubscriberInfo describes an active subscriber.
type SubscriberInfo struct {
	// ID is the caller-provided identifier (empty if not set).
	ID string
	// Topic is the topic this subscriber is on.
	Topic string
	// Scope is the scope key (empty for unscoped subscribers).
	Scope string
	// ConnectedAt is when the subscription was created.
	ConnectedAt time.Time
	// Meta is caller-provided key-value metadata.
	Meta map[string]string
}

// SubscribeMeta holds optional metadata for [SSEBroker.SubscribeWithMeta].
type SubscribeMeta struct {
	// ID is an identifier for this subscriber (e.g., session ID, user ID).
	ID string
	// Meta is arbitrary key-value metadata.
	Meta map[string]string
}

// SubscribeWithMeta registers a subscriber with optional metadata.
// Behaves like [SSEBroker.Subscribe] but attaches metadata queryable
// via [SSEBroker.Subscribers].
func (b *SSEBroker) SubscribeWithMeta(topic string, meta SubscribeMeta) (msgs <-chan string, unsubscribe func()) {
	ch, unsub := b.Subscribe(topic)
	// Enrich the stored info. We need the bidirectional channel to look it up.
	// Since Subscribe just created it and stored it in topics[topic], we can
	// find it by iterating the topic map for the entry whose info has no ID yet
	// and whose ConnectedAt is closest to now. However, the simplest correct
	// approach: iterate topics[topic] looking for the channel whose SubscriberInfo
	// matches the read-only channel we got back.
	b.mu.Lock()
	for c := range b.topics[topic] {
		// A read-only channel derived from a bidirectional channel compares
		// equal when converted. We use a type-safe comparison via the info pointer.
		if info, ok := b.subscriberMeta[c]; ok && info.ID == "" && (<-chan string)(c) == ch {
			info.ID = meta.ID
			info.Meta = meta.Meta
			break
		}
	}
	b.mu.Unlock()
	return ch, unsub
}

// Subscribers returns a snapshot of all active subscribers for the given
// topic. The returned slice is a copy and safe to read without
// synchronization.
func (b *SSEBroker) Subscribers(topic string) []SubscriberInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var result []SubscriberInfo
	for ch := range b.topics[topic] {
		if info, ok := b.subscriberMeta[ch]; ok {
			cp := *info
			if info.Meta != nil {
				cp.Meta = make(map[string]string, len(info.Meta))
				for k, v := range info.Meta {
					cp.Meta[k] = v
				}
			}
			result = append(result, cp)
		}
	}
	for ch := range b.scopedTopics[topic] {
		if info, ok := b.subscriberMeta[ch]; ok {
			cp := *info
			if info.Meta != nil {
				cp.Meta = make(map[string]string, len(info.Meta))
				for k, v := range info.Meta {
					cp.Meta[k] = v
				}
			}
			result = append(result, cp)
		}
	}
	return result
}

// Disconnect closes the subscriber channel with the given ID on the
// given topic. Returns true if the subscriber was found and disconnected.
func (b *SSEBroker) Disconnect(topic, subscriberID string) bool {
	b.mu.Lock()
	// Search unscoped subscribers.
	for ch := range b.topics[topic] {
		if info, ok := b.subscriberMeta[ch]; ok && info.ID == subscriberID {
			delete(b.topics[topic], ch)
			delete(b.subscriberMeta, ch)
			close(ch)
			total := len(b.topics[topic]) + len(b.scopedTopics[topic])
			var lastHooks []func(string)
			if total == 0 {
				lastHooks = b.onLast[topic]
				b.topicEmpty[topic] = time.Now()
			}
			b.mu.Unlock()
			if b.evictThreshold > 0 {
				b.dropCountsMu.Lock()
				delete(b.dropCounts, ch)
				b.dropCountsMu.Unlock()
			}
			for _, fn := range lastHooks {
				go fn(topic)
			}
			b.publishConnectionEvent("disconnect", topic, -1)
			return true
		}
	}
	// Search scoped subscribers.
	for ch := range b.scopedTopics[topic] {
		if info, ok := b.subscriberMeta[ch]; ok && info.ID == subscriberID {
			delete(b.scopedTopics[topic], ch)
			delete(b.subscriberMeta, ch)
			close(ch)
			total := len(b.topics[topic]) + len(b.scopedTopics[topic])
			var lastHooks []func(string)
			if total == 0 {
				lastHooks = b.onLast[topic]
				b.topicEmpty[topic] = time.Now()
			}
			b.mu.Unlock()
			if b.evictThreshold > 0 {
				b.dropCountsMu.Lock()
				delete(b.dropCounts, ch)
				b.dropCountsMu.Unlock()
			}
			for _, fn := range lastHooks {
				go fn(topic)
			}
			b.publishConnectionEvent("disconnect", topic, -1)
			return true
		}
	}
	b.mu.Unlock()
	return false
}

// publishConnectionEvent publishes a JSON event to the meta topic.
// Skips if connection events are disabled or if the topic IS the meta topic.
func (b *SSEBroker) publishConnectionEvent(event, topic string, subCount int) {
	if b.connEventsTopic == "" || topic == b.connEventsTopic {
		return
	}
	if subCount < 0 {
		b.mu.RLock()
		subCount = len(b.topics[topic]) + len(b.scopedTopics[topic])
		b.mu.RUnlock()
	}
	msg, _ := json.Marshal(map[string]any{
		"event":       event,
		"topic":       topic,
		"subscribers": subCount,
	})
	b.Publish(b.connEventsTopic, string(msg))
}
