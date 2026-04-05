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
	"hash/fnv"
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
	replayCache       map[string][]string       // topic → recent messages (ring buffer)
	replaySize        map[string]int            // topic → max messages to keep
	replayLog         map[string][]ReplayEntry  // topic → ordered log with IDs for Last-Event-ID resumption
	lastHash          map[string]uint64 // topic → FNV hash of last published message via PublishIfChanged
	onFirst           map[string][]func(string)
	onLast            map[string][]func(string)
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
	dropOldest        bool
	debounce          debouncer
	throttle          throttler
	metrics           *metricsState // nil when metrics disabled
	evictThreshold    int                          // 0 = disabled
	evictCallback     func(topic string)           // optional callback on eviction
	dropCounts        map[chan string]*atomic.Int64 // per-channel consecutive drop counter
	dropCountsMu      sync.RWMutex                 // separate lock for drop counts (avoid nesting with main mu)
	subscriberMeta    map[chan string]*SubscriberInfo  // channel → subscriber info
	filterPredicates  map[chan string]FilterPredicate // channel → optional message filter
	connEventsTopic   string                         // empty = connection events disabled
	onReplayGap       map[string][]ReplayGapCallback  // topic → gap callbacks
	replayGapStrategy map[string]GapStrategy          // topic → gap strategy
	replayGapSnapshot map[string]func() string        // topic → snapshot func for gap fallback
	groups            map[string]*groupDef           // static topic groups
	groupsMu          sync.RWMutex                   // protects groups
	dynGroups         map[string]*dynamicGroupDef    // dynamic topic groups
	dynGroupsMu       sync.RWMutex                   // protects dynGroups
	middlewares       []Middleware                    // global publish middleware
	topicMiddlewares  []topicMiddleware               // topic-scoped publish middleware
	onRenderError     func(*RenderError)             // nil = no callback
	afterHooks        map[string][]func()              // topic → After hook callbacks
	mutateHooks       map[string][]func(MutationEvent) // resource → OnMutate handlers
	activeChains      sync.Map                         // goroutine ID → *afterChain for cycle detection
	ttlSweeperOnce    sync.Once                      // ensures TTL sweeper starts at most once
	msgTTLSweep       time.Duration                  // override for TTL sweep interval (0 = default 1s)
	rateLimit         *rateLimiter                     // per-subscriber rate limiting
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

// WithDropOldest changes the subscriber buffer strategy from drop-newest
// (default) to drop-oldest. When a subscriber's buffer is full, the oldest
// buffered message is discarded to make room for the new one. This is useful
// for dashboards where the latest data is always more relevant than queued
// historical data.
func WithDropOldest() BrokerOption {
	return func(b *SSEBroker) {
		b.dropOldest = true
	}
}

// WithSlowSubscriberEviction enables automatic disconnection of subscribers
// that have dropped more than threshold consecutive messages. When a subscriber
// is evicted, its channel is closed, triggering the client's EventSource to
// reconnect. The counter resets when a message is successfully delivered.
// A threshold of 0 disables eviction (the default).
func WithSlowSubscriberEviction(threshold int) BrokerOption {
	return func(b *SSEBroker) {
		b.evictThreshold = threshold
	}
}

// WithSlowSubscriberCallback sets a function that is called when a subscriber
// is evicted due to slow consumption. The callback receives the topic name
// and runs in its own goroutine.
func WithSlowSubscriberCallback(fn func(topic string)) BrokerOption {
	return func(b *SSEBroker) {
		b.evictCallback = fn
	}
}

// WithMessageTTLSweep sets the interval at which the background goroutine
// checks for expired TTL entries in the replay cache. Default is 1 second.
// A shorter interval provides faster expiry at the cost of more frequent
// lock acquisitions. This option only takes effect when TTL publishes are used.
func WithMessageTTLSweep(interval time.Duration) BrokerOption {
	return func(b *SSEBroker) {
		b.msgTTLSweep = interval
	}
}

// WithConnectionEvents enables publishing subscriber connect/disconnect
// events to the given meta topic. Events are JSON-formatted messages
// containing the event type, topic, and current subscriber count.
// The meta topic itself does not generate recursive events.
func WithConnectionEvents(metaTopic string) BrokerOption {
	return func(b *SSEBroker) {
		b.connEventsTopic = metaTopic
	}
}

// NewSSEBroker creates a ready-to-use [SSEBroker] with no active topics or
// subscribers. It accepts optional [BrokerOption] values to override defaults.
func NewSSEBroker(opts ...BrokerOption) *SSEBroker {
	b := &SSEBroker{
		topics:         make(map[string]map[chan string]struct{}),
		scopedTopics:   make(map[string]map[chan string]scopedSub),
		replayCache:    make(map[string][]string),
		replaySize:     make(map[string]int),
		replayLog:      make(map[string][]ReplayEntry),
		topicEmpty:     make(map[string]time.Time),
		lastHash:       make(map[string]uint64),
		onFirst:        make(map[string][]func(string)),
		onLast:         make(map[string][]func(string)),
		subscriberMeta:    make(map[chan string]*SubscriberInfo),
		onReplayGap:       make(map[string][]ReplayGapCallback),
		replayGapStrategy: make(map[string]GapStrategy),
		replayGapSnapshot: make(map[string]func() string),
		afterHooks:        make(map[string][]func()),
		mutateHooks:       make(map[string][]func(MutationEvent)),
		bufferSize:        10,
		done:              make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	initGroupFields(b)
	b.debounce.timers = make(map[string]*debounceEntry)
	b.throttle.state = make(map[string]*throttleEntry)
	b.rateLimit = newRateLimiter()
	if b.evictThreshold > 0 {
		b.dropCounts = make(map[chan string]*atomic.Int64)
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
	b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, ConnectedAt: time.Now()}
	total := len(b.topics[topic]) + len(b.scopedTopics[topic])
	var firstHooks []func(string)
	if total == 1 {
		firstHooks = b.onFirst[topic]
	}
	if b.metrics != nil {
		if total > b.metrics.peakSubs[topic] {
			b.metrics.peakSubs[topic] = total
		}
	}
	if b.evictThreshold > 0 {
		b.dropCountsMu.Lock()
		b.dropCounts[ch] = &atomic.Int64{}
		b.dropCountsMu.Unlock()
	}
	replayMsgs := append([]string(nil), b.replayCache[topic]...)
	delete(b.topicEmpty, topic)
	b.mu.Unlock()
	for _, fn := range firstHooks {
		go fn(topic)
	}
	b.publishConnectionEvent("subscribe", topic, total)
	for _, msg := range replayMsgs {
		select {
		case ch <- msg:
		default:
		}
	}
	return ch, func() {
		b.mu.Lock()
		var lastHooks []func(string)
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
			delete(b.subscriberMeta, ch)
			delete(b.filterPredicates, ch)
			close(ch)
			total := len(b.topics[topic]) + len(b.scopedTopics[topic])
			if total == 0 {
				lastHooks = b.onLast[topic]
				b.topicEmpty[topic] = time.Now()
			}
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
		b.publishConnectionEvent("unsubscribe", topic, -1)
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
	b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, Scope: scope, ConnectedAt: time.Now()}
	total := len(b.topics[topic]) + len(b.scopedTopics[topic])
	var firstHooks []func(string)
	if total == 1 {
		firstHooks = b.onFirst[topic]
	}
	if b.metrics != nil {
		if total > b.metrics.peakSubs[topic] {
			b.metrics.peakSubs[topic] = total
		}
	}
	if b.evictThreshold > 0 {
		b.dropCountsMu.Lock()
		b.dropCounts[ch] = &atomic.Int64{}
		b.dropCountsMu.Unlock()
	}
	replayMsgs := append([]string(nil), b.replayCache[topic]...)
	delete(b.topicEmpty, topic)
	b.mu.Unlock()
	for _, fn := range firstHooks {
		go fn(topic)
	}
	b.publishConnectionEvent("subscribe", topic, total)
	for _, msg := range replayMsgs {
		select {
		case ch <- msg:
		default:
		}
	}
	return ch, func() {
		b.mu.Lock()
		var lastHooks []func(string)
		if _, ok := b.scopedTopics[topic][ch]; ok {
			delete(b.scopedTopics[topic], ch)
			delete(b.subscriberMeta, ch)
			delete(b.filterPredicates, ch)
			close(ch)
			total := len(b.topics[topic]) + len(b.scopedTopics[topic])
			if total == 0 {
				lastHooks = b.onLast[topic]
				b.topicEmpty[topic] = time.Now()
			}
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
		b.publishConnectionEvent("unsubscribe", topic, -1)
	}
}

// OnFirstSubscriber registers a callback that fires when the given topic goes
// from zero to one total subscribers (counting both unscoped and scoped). The
// callback runs in its own goroutine and does not block Subscribe. Multiple
// hooks per topic are allowed and all will fire. Hooks persist across
// subscriber cycles. Calling this on a closed broker is a no-op.
func (b *SSEBroker) OnFirstSubscriber(topic string, fn func(topic string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.onFirst[topic] = append(b.onFirst[topic], fn)
}

// OnLastUnsubscribe registers a callback that fires when the given topic goes
// from one to zero total subscribers (counting both unscoped and scoped). The
// callback runs in its own goroutine and does not block the unsubscribe call.
// Multiple hooks per topic are allowed and all will fire. Hooks persist across
// subscriber cycles. Calling this on a closed broker is a no-op.
func (b *SSEBroker) OnLastUnsubscribe(topic string, fn func(topic string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.onLast[topic] = append(b.onLast[topic], fn)
}

// OnRenderError registers a callback that fires when a render function fails.
// This applies to lazy OOB renders and scheduled section renders. Only one
// callback is supported; subsequent calls replace the previous callback.
// The callback receives a [*RenderError] with structured information about
// the failure. The callback runs synchronously in the goroutine where the
// error occurred — avoid blocking operations.
func (b *SSEBroker) OnRenderError(fn func(*RenderError)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onRenderError = fn
}

// fireRenderError invokes the registered render error callback, if any.
func (b *SSEBroker) fireRenderError(re *RenderError) {
	b.mu.RLock()
	fn := b.onRenderError
	b.mu.RUnlock()
	if fn != nil {
		fn(re)
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

// send attempts to send msg on ch. If the channel is full and dropOldest is
// enabled, it drains the oldest message first. Returns true if sent, false if
// dropped.
// send attempts to send msg on ch. If the channel is full and dropOldest is
// enabled, it drains the oldest message first and counts the drop. Returns
// true if the new message was sent, false if it was dropped entirely.
// filterPredicate returns the filter predicate for the given channel, or nil
// if no filter is set. The caller must not hold b.mu.
func (b *SSEBroker) filterPredicate(ch chan string) FilterPredicate {
	b.mu.RLock()
	pred := b.filterPredicates[ch]
	b.mu.RUnlock()
	return pred
}

func (b *SSEBroker) send(ch chan string, msg string) bool {
	if b.dropOldest {
		select {
		case ch <- msg:
			return true
		default:
			// Channel full — drain oldest and count it as a drop.
			select {
			case <-ch:
				b.drops.Add(1)
			default:
			}
			// Try again.
			select {
			case ch <- msg:
				return true
			default:
				return false
			}
		}
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

// publishToChannels fans out msg to the given channels, tracking drops and
// evicting slow subscribers when configured.
func (b *SSEBroker) publishToChannels(topic string, channels []chan string, msg string) (sent, dropped int) {
	for _, ch := range channels {
		func() {
			defer func() { _ = recover() }()
			// Per-subscriber rate limiting: hold the message if within interval.
			if b.rateLimit.hold(ch, msg) {
				sent++ // held messages count as sent, not dropped
				return
			}
			// Check filter predicate — non-matching messages are silently
			// skipped without counting toward drops or backpressure.
			if pred := b.filterPredicate(ch); pred != nil && !pred(msg) {
				return
			}
			if b.send(ch, msg) {
				sent++
				if b.evictThreshold > 0 {
					b.resetDropCount(ch)
				}
			} else {
				b.drops.Add(1)
				dropped++
				if b.evictThreshold > 0 {
					if b.incrementDropCount(ch) >= int64(b.evictThreshold) {
						b.evictSubscriber(topic, ch)
					}
				}
			}
		}()
	}
	return
}

func (b *SSEBroker) resetDropCount(ch chan string) {
	b.dropCountsMu.RLock()
	counter, ok := b.dropCounts[ch]
	b.dropCountsMu.RUnlock()
	if ok {
		counter.Store(0)
	}
}

func (b *SSEBroker) incrementDropCount(ch chan string) int64 {
	b.dropCountsMu.RLock()
	counter, ok := b.dropCounts[ch]
	b.dropCountsMu.RUnlock()
	if !ok {
		return 0
	}
	return counter.Add(1)
}

func (b *SSEBroker) evictSubscriber(topic string, ch chan string) {
	b.mu.Lock()
	if _, ok := b.topics[topic][ch]; ok {
		delete(b.topics[topic], ch)
		delete(b.subscriberMeta, ch)
		delete(b.filterPredicates, ch)
		close(ch)
		total := len(b.topics[topic]) + len(b.scopedTopics[topic])
		var lastHooks []func(string)
		if total == 0 {
			lastHooks = b.onLast[topic]
			b.topicEmpty[topic] = time.Now()
		}
		b.mu.Unlock()
		for _, fn := range lastHooks {
			go fn(topic)
		}
	} else if _, ok := b.scopedTopics[topic][ch]; ok {
		delete(b.scopedTopics[topic], ch)
		delete(b.subscriberMeta, ch)
		delete(b.filterPredicates, ch)
		close(ch)
		total := len(b.topics[topic]) + len(b.scopedTopics[topic])
		var lastHooks []func(string)
		if total == 0 {
			lastHooks = b.onLast[topic]
			b.topicEmpty[topic] = time.Now()
		}
		b.mu.Unlock()
		for _, fn := range lastHooks {
			go fn(topic)
		}
	} else {
		b.mu.Unlock()
	}
	// Clean up drop counter.
	b.dropCountsMu.Lock()
	delete(b.dropCounts, ch)
	b.dropCountsMu.Unlock()
	// Fire callback.
	if b.evictCallback != nil {
		go b.evictCallback(topic)
	}
}

// Publish fans out msg to every subscriber of the given topic. It is
// non-blocking: if a subscriber's channel buffer is full, the message is
// silently dropped for that subscriber rather than blocking the publisher.
// Publishing to a topic with no subscribers is a no-op.
func (b *SSEBroker) Publish(topic, msg string) {
	dispatch := b.applyMiddleware(topic, b.publishDirect)
	dispatch(topic, msg)
}

// publishDirect fans out msg to unscoped subscribers without applying
// middleware.  It is the terminal function in the middleware chain for
// [SSEBroker.Publish].
func (b *SSEBroker) publishDirect(topic, msg string) {
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

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
	b.fireAfterHooks(topic)
}

// PublishWithReplay behaves like [SSEBroker.Publish] but also caches msg so
// that future subscribers of the topic immediately receive it on connect. Only
// the most recent message per topic is retained. Use [SSEBroker.ClearReplay]
// to remove the cached message for a topic.
func (b *SSEBroker) PublishWithReplay(topic, msg string) {
	dispatch := b.applyMiddleware(topic, b.publishWithReplayDirect)
	dispatch(topic, msg)
}

// publishWithReplayDirect caches msg for replay and fans it out to
// unscoped subscribers without applying middleware.
func (b *SSEBroker) publishWithReplayDirect(topic, msg string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	maxSize := 1
	if n, ok := b.replaySize[topic]; ok {
		maxSize = n
	}
	msgs := b.replayCache[topic]
	msgs = append(msgs, msg)
	if len(msgs) > maxSize {
		msgs = msgs[len(msgs)-maxSize:]
	}
	b.replayCache[topic] = msgs
	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
	b.fireAfterHooks(topic)
}

// SetReplayPolicy sets how many recent messages to cache for replay on the
// given topic. New subscribers receive up to n cached messages in order before
// live messages. Use n=0 to disable replay for the topic.
func (b *SSEBroker) SetReplayPolicy(topic string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 {
		delete(b.replaySize, topic)
		delete(b.replayCache, topic)
		delete(b.replayLog, topic)
		delete(b.replayGapStrategy, topic)
		delete(b.replayGapSnapshot, topic)
		return
	}
	b.replaySize[topic] = n
	if msgs := b.replayCache[topic]; len(msgs) > n {
		b.replayCache[topic] = msgs[len(msgs)-n:]
	}
	if logs := b.replayLog[topic]; len(logs) > n {
		b.replayLog[topic] = logs[len(logs)-n:]
	}
}

// ClearReplay removes the cached replay message for the given topic. Future
// subscribers will no longer receive a replayed message on connect.
func (b *SSEBroker) ClearReplay(topic string) {
	b.mu.Lock()
	delete(b.replayCache, topic)
	delete(b.replaySize, topic)
	delete(b.replayLog, topic)
	delete(b.replayGapStrategy, topic)
	delete(b.replayGapSnapshot, topic)
	b.mu.Unlock()
}

// PublishTo fans out msg only to scoped subscribers of the given topic whose
// scope matches. It is non-blocking: if a subscriber's channel buffer is full,
// the message is silently dropped. Publishing to a topic or scope with no
// matching subscribers is a no-op.
func (b *SSEBroker) PublishTo(topic, scope, msg string) {
	dispatch := b.applyMiddleware(topic, func(t, m string) {
		b.publishToDirect(t, scope, m)
	})
	dispatch(topic, msg)
}

// publishToDirect fans out msg to scoped subscribers without applying
// middleware.
func (b *SSEBroker) publishToDirect(topic, scope, msg string) {
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

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
	b.fireAfterHooks(topic)
}

// hashMsg returns the FNV-64a hash of msg.
func hashMsg(msg string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(msg)) //nolint:errcheck // hash.Write never returns an error
	return h.Sum64()
}

// PublishIfChanged publishes msg to the given topic only when it differs from
// the last message published via PublishIfChanged for that topic. It returns
// true if the message was published (content changed) or false if it was
// skipped (identical to the previous message). Comparison is done via an
// FNV-64a hash of the message content.
//
// The deduplication state is per-topic and independent of [SSEBroker.Publish].
// Use [SSEBroker.ClearDedup] to reset the stored hash for a topic.
func (b *SSEBroker) PublishIfChanged(topic, msg string) bool {
	h := hashMsg(msg)

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
	b.mu.Unlock()

	dispatch := b.applyMiddleware(topic, b.publishDirect)
	dispatch(topic, msg)
	return true
}

// PublishIfChangedTo publishes msg to scoped subscribers of the given topic
// only when it differs from the last message published via PublishIfChangedTo
// for that topic+scope combination. Returns true if published, false if skipped.
// Comparison is done via an FNV-64a hash of the message content.
func (b *SSEBroker) PublishIfChangedTo(topic, scope, msg string) bool {
	key := topic + "\x00" + scope
	h := hashMsg(msg)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	if prev, ok := b.lastHash[key]; ok && prev == h {
		b.mu.Unlock()
		return false
	}
	b.lastHash[key] = h
	b.mu.Unlock()

	dispatch := b.applyMiddleware(topic, func(t, m string) {
		b.publishToDirect(t, scope, m)
	})
	dispatch(topic, msg)
	return true
}

// ClearDedup resets the deduplication state for the given topic so the next
// call to [SSEBroker.PublishIfChanged] will always publish regardless of
// content.
func (b *SSEBroker) ClearDedup(topic string) {
	b.mu.Lock()
	delete(b.lastHash, topic)
	b.mu.Unlock()
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

// SetRetry sends an SSE retry directive to all subscribers of the given topic,
// including both unscoped and scoped subscribers. The browser's EventSource
// stores this value and uses it for the next reconnect attempt. Call before
// [SSEBroker.Close] in a graceful shutdown sequence to prevent clients from
// thundering-herding against new pods.
func (b *SSEBroker) SetRetry(topic string, d time.Duration) {
	msg := fmt.Sprintf("retry: %d\n\n", d.Milliseconds())
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.topics[topic] {
		select {
		case ch <- msg:
		default:
		}
	}
	for ch := range b.scopedTopics[topic] {
		select {
		case ch <- msg:
		default:
		}
	}
}

// SetRetryAll sends an SSE retry directive to all subscribers across all
// topics, including both unscoped and scoped subscribers. This is a
// convenience for graceful shutdown scenarios where every connected client
// should back off before reconnecting.
func (b *SSEBroker) SetRetryAll(d time.Duration) {
	msg := fmt.Sprintf("retry: %d\n\n", d.Milliseconds())
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
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.done)
	b.replayCache = nil
	b.replaySize = nil
	b.replayLog = nil
	b.lastHash = nil
	b.subscriberMeta = nil
	b.filterPredicates = nil
	b.onReplayGap = nil
	b.replayGapStrategy = nil
	b.replayGapSnapshot = nil
	b.afterHooks = nil
	b.mutateHooks = nil
	if b.dropCounts != nil {
		b.dropCountsMu.Lock()
		b.dropCounts = nil
		b.dropCountsMu.Unlock()
	}
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
	b.mu.Unlock()
	b.debounce.mu.Lock()
	for topic, entry := range b.debounce.timers {
		entry.timer.Stop()
		delete(b.debounce.timers, topic)
	}
	b.debounce.mu.Unlock()
	b.throttle.mu.Lock()
	for topic, entry := range b.throttle.state {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		delete(b.throttle.state, topic)
	}
	b.throttle.mu.Unlock()
	b.rateLimit.stop()
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
