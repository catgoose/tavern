package tavern

import (
	"encoding/json"
	"time"
)

// SubscriberInfo describes an active subscriber. Retrieve a snapshot of all
// subscribers for a topic via [SSEBroker.Subscribers].
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
// The ID is used for targeted operations like [SSEBroker.Disconnect] and
// [SSEBroker.AddTopic].
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
	return b.subscribeLive(topic, subSpec{meta: &meta}, false)
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
	var target chan string
	for ch := range b.topics[topic] {
		if info, ok := b.subscriberMeta[ch]; ok && info.ID == subscriberID {
			target = ch
			break
		}
	}
	if target == nil {
		for ch := range b.scopedTopics[topic] {
			if info, ok := b.subscriberMeta[ch]; ok && info.ID == subscriberID {
				target = ch
				break
			}
		}
	}
	if target == nil {
		b.mu.Unlock()
		return false
	}
	l := b.removeLocked(topic, target)
	b.mu.Unlock()
	b.finishLeave(topic, target, l)
	b.publishConnectionEvent("disconnect", topic, -1)
	return true
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
