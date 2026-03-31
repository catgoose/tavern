// Package tavern provides a thread-safe, topic-based pub/sub broker for
// Server-Sent Events (SSE). It is designed for fan-out messaging where a
// server publishes events and multiple HTTP clients consume them via SSE
// streams.
//
// All broker methods are safe for concurrent use by multiple goroutines.
package tavern

import (
	"sync"
)

// SSEBroker is a thread-safe, topic-based pub/sub message broker. Subscribers
// receive messages on a buffered channel and publishers fan out messages to all
// subscribers of a given topic. A zero-value SSEBroker is not usable; create
// one with [NewSSEBroker].
type SSEBroker struct {
	topics     map[string]map[chan string]struct{}
	mu         sync.RWMutex
	bufferSize int
}

// BrokerOption configures the SSE broker.
type BrokerOption func(*SSEBroker)

// WithBufferSize sets the subscriber channel buffer size. Default is 10.
func WithBufferSize(size int) BrokerOption {
	return func(b *SSEBroker) {
		b.bufferSize = size
	}
}

// NewSSEBroker creates a ready-to-use [SSEBroker] with no active topics or
// subscribers. It accepts optional [BrokerOption] values to override defaults.
func NewSSEBroker(opts ...BrokerOption) *SSEBroker {
	b := &SSEBroker{
		topics:     make(map[string]map[chan string]struct{}),
		bufferSize: 10,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Subscribe registers a new subscriber for the given topic and returns a
// read-only channel that will receive published messages, along with an
// unsubscribe function. The caller must invoke the returned function when done
// to release resources and close the channel. Calling the unsubscribe function
// more than once is safe and has no effect after the first call.
//
// The returned channel is buffered (default capacity 10, configurable via
// [WithBufferSize]). If the subscriber does not drain the channel fast enough,
// messages will be dropped by [SSEBroker.Publish].
func (b *SSEBroker) Subscribe(topic string) (<-chan string, func()) {
	ch := make(chan string, b.bufferSize)
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

// HasSubscribers reports whether the given topic has at least one active
// subscriber. This is useful for skipping expensive serialization when no
// clients are listening.
func (b *SSEBroker) HasSubscribers(topic string) bool {
	b.mu.RLock()
	n := len(b.topics[topic])
	b.mu.RUnlock()
	return n > 0
}

// TopicCounts returns a snapshot of the number of active subscribers per topic.
// The returned map is a copy and safe to read without synchronization.
func (b *SSEBroker) TopicCounts() map[string]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := make(map[string]int, len(b.topics))
	for topic, subs := range b.topics {
		counts[topic] = len(subs)
	}
	return counts
}

// Publish fans out msg to every subscriber of the given topic. It is
// non-blocking: if a subscriber's channel buffer is full, the message is
// silently dropped for that subscriber rather than blocking the publisher.
// Publishing to a topic with no subscribers is a no-op.
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

// Close shuts down the broker by closing all subscriber channels and removing
// all topics. After Close returns, any pending reads on subscriber channels
// will receive the zero value. It is safe to call Close while other goroutines
// are publishing or subscribing; however, no new messages will be delivered
// after Close returns.
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
