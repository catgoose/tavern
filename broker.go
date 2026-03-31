// Package tavern provides a topic-based pub/sub broker for Server-Sent Events.
package tavern

import (
	"sync"
)

// SSEBroker manages topic-based pub/sub for SSE events.
type SSEBroker struct {
	topics map[string]map[chan string]struct{}
	mu     sync.RWMutex
}

// NewSSEBroker creates a new SSEBroker instance.
func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		topics: make(map[string]map[chan string]struct{}),
	}
}

// Subscribe returns a read channel for a topic and an unsubscribe function.
// The caller must invoke the returned function to release resources.
func (b *SSEBroker) Subscribe(topic string) (<-chan string, func()) {
	ch := make(chan string, 10)
	b.mu.Lock()
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[chan string]struct{})
	}
	b.topics[topic][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// HasSubscribers reports whether the given topic has at least one subscriber.
func (b *SSEBroker) HasSubscribers(topic string) bool {
	b.mu.RLock()
	n := len(b.topics[topic])
	b.mu.RUnlock()
	return n > 0
}

// TopicCounts returns the number of active subscribers per topic.
func (b *SSEBroker) TopicCounts() map[string]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := make(map[string]int, len(b.topics))
	for topic, subs := range b.topics {
		counts[topic] = len(subs)
	}
	return counts
}

// Publish sends a message to all subscribers of a topic.
// Non-blocking: if a subscriber channel is full, the message is skipped for that subscriber.
func (b *SSEBroker) Publish(topic, msg string) {
	b.mu.RLock()
	subscribers, exists := b.topics[topic]
	if !exists || len(subscribers) == 0 {
		b.mu.RUnlock()
		return
	}
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.RUnlock()

	for _, ch := range channels {
		// Channel may have been closed by unsub() between the snapshot and now.
		// Recover from the resulting panic rather than adding complex synchronization.
		func() {
			defer func() { _ = recover() }()
			select {
			case ch <- msg:
			default:
				// channel full — skip stale subscriber
			}
		}()
	}
}

// Close drains all topic subscriber maps for clean shutdown.
func (b *SSEBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for topic, subs := range b.topics {
		for ch := range subs {
			delete(subs, ch)
			close(ch)
		}
		delete(b.topics, topic)
	}
}
