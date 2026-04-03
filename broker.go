// Package tavern provides a thread-safe, topic-based pub/sub broker for
// Server-Sent Events (SSE). It is designed for fan-out messaging where a
// server publishes events and multiple HTTP clients consume them via SSE
// streams.
//
// All broker methods are safe for concurrent use by multiple goroutines.
package tavern

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// SSEBroker is a thread-safe, topic-based pub/sub message broker. Subscribers
// receive messages on a buffered channel and publishers fan out messages to all
// subscribers of a given topic. A zero-value SSEBroker is not usable; create
// one with [NewSSEBroker].
type SSEBroker struct {
	topics            map[string]map[chan string]struct{}
	scopedTopics      map[string]map[chan string]scopedSub
	replayCache       map[string]string
	mu                sync.RWMutex
	bufferSize        int
	drops             atomic.Int64
	closed            bool
	publishers        sync.WaitGroup
	logger            *slog.Logger
	keepaliveInterval time.Duration
	topicTTL          time.Duration
	topicEmpty        map[string]time.Time
	done              chan struct{}
}

// Topic name constants are conventions for common real-time use cases.
// Any string works as a topic name; these are provided for consistent naming.
const (
	TopicActivityFeed = "activity-feed"
)

// scopedSub tracks the scope key alongside the channel.
type scopedSub struct {
	scope string
}

// PublisherFunc is a long-running function that publishes messages to the broker.
// It receives the broker's context and should return when the context is cancelled.
type PublisherFunc func(ctx context.Context)

// BrokerOption configures the SSE broker.
type BrokerOption func(*SSEBroker)

// WithBufferSize sets the subscriber channel buffer size. Default is 10.
func WithBufferSize(size int) BrokerOption {
	return func(b *SSEBroker) {
		b.bufferSize = size
	}
}

// WithLogger sets a structured logger for the broker. When set, publisher
// panics and errors are logged. Default is nil (no logging).
func WithLogger(l *slog.Logger) BrokerOption {
	return func(b *SSEBroker) {
		b.logger = l
	}
}

// WithKeepalive enables periodic SSE comment keepalives sent to all
// subscribers at the given interval. This keeps connections alive through
// proxies and load balancers that close idle connections. A zero or negative
// interval disables keepalives (the default).
func WithKeepalive(interval time.Duration) BrokerOption {
	return func(b *SSEBroker) {
		b.keepaliveInterval = interval
	}
}

// WithTopicTTL sets how long a topic with zero subscribers may remain in the
// broker before it is automatically removed. A background goroutine sweeps at
// half the TTL interval. A zero or negative TTL disables auto-cleanup.
func WithTopicTTL(ttl time.Duration) BrokerOption {
	return func(b *SSEBroker) {
		b.topicTTL = ttl
	}
}

// NewSSEBroker creates a ready-to-use [SSEBroker] with no active topics or
// subscribers. It accepts optional [BrokerOption] values to override defaults.
func NewSSEBroker(opts ...BrokerOption) *SSEBroker {
	b := &SSEBroker{
		topics:       make(map[string]map[chan string]struct{}),
		scopedTopics: make(map[string]map[chan string]scopedSub),
		replayCache:  make(map[string]string),
		topicEmpty:   make(map[string]time.Time),
		bufferSize:   10,
		done:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.keepaliveInterval > 0 {
		go b.keepaliveLoop()
	}
	if b.topicTTL > 0 {
		go b.startTopicSweep()
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
func (b *SSEBroker) Subscribe(topic string) (msgs <-chan string, unsubscribe func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan string, b.bufferSize)
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[chan string]struct{})
	}
	b.topics[topic][ch] = struct{}{}
	replay, hasReplay := b.replayCache[topic]
	delete(b.topicEmpty, topic)
	b.mu.Unlock()
	if hasReplay {
		select {
		case ch <- replay:
		default:
		}
	}
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
			close(ch)
			if len(b.topics[topic])+len(b.scopedTopics[topic]) == 0 {
				b.topicEmpty[topic] = time.Now()
			}
		}
		b.mu.Unlock()
	}
}

// SubscribeScoped registers a subscriber with a scope key for the given topic.
// Only messages published via [SSEBroker.PublishTo] with a matching scope will
// be delivered. The returned unsubscribe function releases resources and closes
// the channel; it is safe to call more than once.
func (b *SSEBroker) SubscribeScoped(topic, scope string) (msgs <-chan string, unsubscribe func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan string, b.bufferSize)
	if b.scopedTopics[topic] == nil {
		b.scopedTopics[topic] = make(map[chan string]scopedSub)
	}
	b.scopedTopics[topic][ch] = scopedSub{scope: scope}
	replay, hasReplay := b.replayCache[topic]
	delete(b.topicEmpty, topic)
	b.mu.Unlock()
	if hasReplay {
		select {
		case ch <- replay:
		default:
		}
	}
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.scopedTopics[topic][ch]; ok {
			delete(b.scopedTopics[topic], ch)
			close(ch)
			if len(b.topics[topic])+len(b.scopedTopics[topic]) == 0 {
				b.topicEmpty[topic] = time.Now()
			}
		}
		b.mu.Unlock()
	}
}

// HasSubscribers reports whether the given topic has at least one active
// subscriber, including both unscoped and scoped subscribers. This is useful
// for skipping expensive serialization when no clients are listening.
func (b *SSEBroker) HasSubscribers(topic string) bool {
	b.mu.RLock()
	n := len(b.topics[topic]) + len(b.scopedTopics[topic])
	b.mu.RUnlock()
	return n > 0
}

// TopicCounts returns a snapshot of the number of active subscribers per topic.
// The returned map is a copy and safe to read without synchronization. Counts
// include both unscoped and scoped subscribers.
func (b *SSEBroker) TopicCounts() map[string]int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := make(map[string]int, len(b.topics)+len(b.scopedTopics))
	for topic, subs := range b.topics {
		counts[topic] += len(subs)
	}
	for topic, subs := range b.scopedTopics {
		counts[topic] += len(subs)
	}
	return counts
}

// SubscriberCount returns the total number of active subscribers across all
// topics, including both unscoped and scoped subscribers.
func (b *SSEBroker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, subs := range b.topics {
		n += len(subs)
	}
	for _, subs := range b.scopedTopics {
		n += len(subs)
	}
	return n
}

// PublishDrops returns the cumulative number of messages that were dropped
// because a subscriber's channel buffer was full.
func (b *SSEBroker) PublishDrops() int64 {
	return b.drops.Load()
}

// BrokerStats is a point-in-time summary of broker state returned by
// [SSEBroker.Stats].
type BrokerStats struct {
	// Topics is the number of active topics.
	Topics int
	// Subscribers is the total number of active subscribers across all topics.
	Subscribers int
	// PublishDrops is the cumulative number of dropped messages.
	PublishDrops int64
}

// Stats returns a point-in-time [BrokerStats] snapshot. It is a convenience
// method that combines [SSEBroker.TopicCounts], [SSEBroker.SubscriberCount],
// and [SSEBroker.PublishDrops] into a single lock acquisition.
func (b *SSEBroker) Stats() BrokerStats {
	b.mu.RLock()
	seen := make(map[string]struct{}, len(b.topics)+len(b.scopedTopics))
	subs := 0
	for t, s := range b.topics {
		seen[t] = struct{}{}
		subs += len(s)
	}
	for t, s := range b.scopedTopics {
		seen[t] = struct{}{}
		subs += len(s)
	}
	topics := len(seen)
	b.mu.RUnlock()
	return BrokerStats{
		Topics:       topics,
		Subscribers:  subs,
		PublishDrops: b.drops.Load(),
	}
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
				b.drops.Add(1)
			}
		}()
	}
}

// PublishWithReplay behaves like [SSEBroker.Publish] but also caches msg so
// that future subscribers of the topic immediately receive it on connect. Only
// the most recent message per topic is retained. Use [SSEBroker.ClearReplay]
// to remove the cached message for a topic.
func (b *SSEBroker) PublishWithReplay(topic, msg string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.replayCache[topic] = msg
	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	for _, ch := range channels {
		func() {
			defer func() { _ = recover() }()
			select {
			case ch <- msg:
			default:
				b.drops.Add(1)
			}
		}()
	}
}

// ClearReplay removes the cached replay message for the given topic. Future
// subscribers will no longer receive a replayed message on connect.
func (b *SSEBroker) ClearReplay(topic string) {
	b.mu.Lock()
	delete(b.replayCache, topic)
	b.mu.Unlock()
}

// PublishTo fans out msg only to scoped subscribers of the given topic whose
// scope matches. It is non-blocking: if a subscriber's channel buffer is full,
// the message is silently dropped. Publishing to a topic or scope with no
// matching subscribers is a no-op.
func (b *SSEBroker) PublishTo(topic, scope, msg string) {
	b.mu.RLock()
	scopedSubs, exists := b.scopedTopics[topic]
	if !exists || len(scopedSubs) == 0 {
		b.mu.RUnlock()
		return
	}
	channels := make([]chan string, 0, len(scopedSubs))
	for ch, sub := range scopedSubs {
		if sub.scope == scope {
			channels = append(channels, ch)
		}
	}
	b.mu.RUnlock()

	for _, ch := range channels {
		func() {
			defer func() { _ = recover() }()
			select {
			case ch <- msg:
			default:
				b.drops.Add(1)
			}
		}()
	}
}

// RunPublisher launches fn in a new goroutine with panic recovery. If fn
// panics, the panic is recovered and logged (when a logger is configured via
// [WithLogger]). The goroutine is tracked by the broker's internal wait group
// so that [SSEBroker.Close] blocks until all publishers have returned.
//
// fn receives the provided context and should return when the context is
// cancelled. Callers typically pass a context derived from the application's
// shutdown signal.
func (b *SSEBroker) RunPublisher(ctx context.Context, fn PublisherFunc) {
	b.publishers.Add(1)
	go func() {
		defer b.publishers.Done()
		defer func() {
			if r := recover(); r != nil {
				if b.logger != nil {
					b.logger.Error("publisher panic recovered", "panic", fmt.Sprint(r))
				}
			}
		}()
		fn(ctx)
	}()
}

// keepaliveLoop sends SSE comment keepalives to all subscriber channels at
// the configured interval. It stops when the done channel is closed.
func (b *SSEBroker) keepaliveLoop() {
	ticker := time.NewTicker(b.keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.sendKeepalive()
		}
	}
}

// sendKeepalive sends a keepalive comment to all subscriber channels using a
// non-blocking send. Drops from keepalives are not counted in PublishDrops.
// The read lock is held for the entire operation to prevent Close from closing
// channels mid-send.
func (b *SSEBroker) sendKeepalive() {
	const msg = ": keepalive\n"
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, subs := range b.topics {
		for ch := range subs {
			select {
			case ch <- msg:
			default:
			}
		}
	}
	for _, subs := range b.scopedTopics {
		for ch := range subs {
			select {
			case ch <- msg:
			default:
			}
		}
	}
}

// Close shuts down the broker. It first waits for all publisher goroutines
// started via [SSEBroker.RunPublisher] to return, then closes all subscriber
// channels and removes all topics. After Close returns, any pending reads on
// subscriber channels will receive the zero value. It is safe to call Close
// while other goroutines are publishing or subscribing; however, no new
// messages will be delivered after Close returns.
func (b *SSEBroker) Close() {
	b.publishers.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.done)
	b.replayCache = nil
	for topic, subs := range b.topics {
		for ch := range subs {
			delete(subs, ch)
			close(ch)
		}
		delete(b.topics, topic)
	}
	for topic, subs := range b.scopedTopics {
		for ch := range subs {
			delete(subs, ch)
			close(ch)
		}
		delete(b.scopedTopics, topic)
	}
}

// startTopicSweep periodically removes topics that have had zero subscribers
// for longer than the configured TTL. It runs until the broker is closed.
func (b *SSEBroker) startTopicSweep() {
	ticker := time.NewTicker(b.topicTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case now := <-ticker.C:
			b.mu.Lock()
			for topic, emptyAt := range b.topicEmpty {
				if now.Sub(emptyAt) >= b.topicTTL {
					delete(b.topics, topic)
					delete(b.scopedTopics, topic)
					delete(b.topicEmpty, topic)
				}
			}
			b.mu.Unlock()
		}
	}
}
