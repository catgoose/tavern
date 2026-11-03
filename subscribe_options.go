package tavern

import (
	"sync"
	"time"
)

// subscribeConfig holds the combined options for a composable subscription.
type subscribeConfig struct {
	scope      string
	filter     FilterPredicate
	rate       *Rate
	meta       *SubscribeMeta
	snapshotFn func() string

	disconnectOnLoss      bool
	terminateOnInnerClose bool
}

// SubscribeOption configures a composable subscription created via
// [SSEBroker.SubscribeWith], [SSEBroker.SubscribeMultiWith], or
// [SSEBroker.SubscribeGlobWith].
type SubscribeOption func(*subscribeConfig)

// SubWithScope sets the subscription scope. Only messages published via
// [SSEBroker.PublishTo] with a matching scope will be delivered.
func SubWithScope(scope string) SubscribeOption {
	return func(c *subscribeConfig) {
		c.scope = scope
	}
}

// SubWithFilter attaches a filter predicate. Only messages for which the
// predicate returns true are delivered.
func SubWithFilter(fn FilterPredicate) SubscribeOption {
	return func(c *subscribeConfig) {
		c.filter = fn
	}
}

// SubWithRate enables per-subscriber rate limiting.
func SubWithRate(r Rate) SubscribeOption {
	return func(c *subscribeConfig) {
		c.rate = &r
	}
}

// SubWithMeta attaches subscriber metadata queryable via
// [SSEBroker.Subscribers].
func SubWithMeta(m SubscribeMeta) SubscribeOption {
	return func(c *subscribeConfig) {
		c.meta = &m
	}
}

// SubWithSnapshot provides a snapshot function whose result is delivered as
// the first message before any live publishes.
func SubWithSnapshot(fn func() string) SubscribeOption {
	return func(c *subscribeConfig) {
		c.snapshotFn = fn
	}
}

// SubWithDisconnectOnLoss makes the subscription terminate on the first live
// message the broker cannot queue for it. Instead of dropping the message and
// continuing, the broker removes the subscriber and closes its channel, so a
// later successful delivery can never hide the loss. This is intended for
// invalidation streams, where closure is the signal to reconnect and refetch
// canonical state.
//
// Guarantees and limits:
//
//   - Applies per subscription, independent of [WithSlowSubscriberEviction]
//     thresholds. [WithDropOldest] never discards this subscriber's buffered
//     messages, and [WithAdaptiveBackpressure] tiers (throttle, simplify) do
//     not apply to it. A [SSEBroker.PublishBlocking] timeout also counts as
//     a loss.
//   - Messages rejected by [SubWithFilter] or by scope are not losses.
//   - Closure may follow messages that were already buffered; drain the
//     channel until it is closed. No lossless or exactly-once delivery is
//     promised: termination is the recovery signal.
//   - Only live delivery is covered. Replay and control events written at
//     subscribe time stay best-effort.
//   - [SSEBroker.SubscribeWith] registers the subscription the way
//     [SSEBroker.SubscribeFromIDWith] does with an empty Last-Event-ID:
//     replayed entries published with an ID carry their SSE id line, and a
//     snapshot is followed by the replay cache.
//   - A consumer blocked writing to a slow HTTP client observes closure only
//     after that write returns or times out.
//   - Cannot be combined with [SubWithRate], whose latest-wins holding
//     discards messages by design; that combination panics. Not supported by
//     [SSEBroker.SubscribeGlobWith], which panics if it is set.
//
// In [SSEBroker.SubscribeMultiWith] it also enables
// [SubWithTerminateOnInnerClose], so the loss of any one topic closes the
// aggregate channel.
func SubWithDisconnectOnLoss() SubscribeOption {
	return func(c *subscribeConfig) {
		c.disconnectOnLoss = true
	}
}

// SubWithTerminateOnInnerClose makes a [SSEBroker.SubscribeMultiWith]
// subscription treat every topic as required. When any inner subscription
// closes (eviction, disconnect-on-loss, or broker shutdown), the remaining
// inner subscriptions are unsubscribed and the aggregate channel is closed.
// Messages already in the aggregate buffer stay readable before the close;
// messages still queued on inner subscriptions may be discarded. Termination
// does not wait for the aggregate to be drained. If any topic is refused at
// admission, SubscribeMultiWith releases the topics it already subscribed and
// returns nil channels rather than serving partial coverage.
//
// Without this option, the aggregate stays open until every inner
// subscription has ended. The option has no effect on single-topic or glob
// subscriptions.
func SubWithTerminateOnInnerClose() SubscribeOption {
	return func(c *subscribeConfig) {
		c.terminateOnInnerClose = true
	}
}

// rejectLossyCombinations panics for option combinations that cannot honor
// disconnect-on-loss.
func (c subscribeConfig) rejectLossyCombinations() {
	if c.disconnectOnLoss && c.rate != nil {
		panic("tavern: SubWithDisconnectOnLoss cannot be combined with SubWithRate")
	}
}

func buildSubscribeConfig(opts []SubscribeOption) subscribeConfig {
	var cfg subscribeConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// SubscribeWith creates a subscription using composable options. It combines
// the functionality of Subscribe, SubscribeScoped, SubscribeWithFilter,
// SubscribeWithRate, SubscribeWithMeta, and SubscribeWithSnapshot into a
// single call.
func (b *SSEBroker) SubscribeWith(topic string, opts ...SubscribeOption) (msgs <-chan string, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	cfg.rejectLossyCombinations()

	// Disconnect-on-loss needs the policy registered in the same critical
	// section as the subscriber, which the unified resume path provides.
	if cfg.disconnectOnLoss {
		return b.subscribeFromIDInternal(topic, "", cfg)
	}

	// Pre-compute snapshot outside lock when needed for atomic ordering.
	var preSnap string
	if cfg.snapshotFn != nil {
		preSnap = cfg.snapshotFn()
	}

	var ch <-chan string
	var unsub func()

	switch {
	case cfg.scope != "" && cfg.filter != nil && cfg.snapshotFn != nil:
		ch, unsub = b.subscribeScopedWithFilterAndSnapshot(topic, cfg.scope, cfg.filter, preSnap)
	case cfg.scope != "" && cfg.filter != nil:
		ch, unsub = b.SubscribeScopedWithFilter(topic, cfg.scope, cfg.filter)
	case cfg.scope != "" && cfg.snapshotFn != nil:
		ch, unsub = b.subscribeScopedWithSnapshot(topic, cfg.scope, preSnap)
	case cfg.scope != "":
		ch, unsub = b.SubscribeScoped(topic, cfg.scope)
	case cfg.filter != nil && cfg.snapshotFn != nil:
		ch, unsub = b.subscribeWithFilterAndSnapshot(topic, cfg.filter, preSnap)
	case cfg.filter != nil:
		ch, unsub = b.SubscribeWithFilter(topic, cfg.filter)
	case cfg.snapshotFn != nil:
		ch, unsub = b.SubscribeWithSnapshot(topic, cfg.snapshotFn)
	default:
		ch, unsub = b.Subscribe(topic)
	}

	if ch == nil {
		return nil, nil
	}

	// Apply meta.
	if cfg.meta != nil {
		biCh := b.findBiChanAny(topic, ch)
		if biCh != nil {
			b.mu.Lock()
			if info, ok := b.subscriberMeta[biCh]; ok {
				info.ID = cfg.meta.ID
				info.Meta = cfg.meta.Meta
			}
			b.mu.Unlock()
		}
	}

	// Apply rate limiting.
	if cfg.rate != nil {
		interval := cfg.rate.interval()
		if interval > 0 {
			biCh := b.findBiChanAny(topic, ch)
			if biCh != nil {
				b.rateLimit.add(biCh, topic, interval)
				origUnsub := unsub
				unsub = func() {
					b.rateLimit.remove(biCh)
					origUnsub()
				}
			}
		}
	}

	return ch, unsub
}

// findBiChanAny searches both topics and scopedTopics for the bidirectional
// channel matching the read-only channel.
func (b *SSEBroker) findBiChanAny(topic string, readOnly <-chan string) chan string {
	if ch := b.findBiChan(topic, readOnly); ch != nil {
		return ch
	}
	return b.findBiScopedChan(topic, readOnly)
}

// SubscribeMultiWith subscribes to multiple topics with composable options.
// Options like filter and rate are applied uniformly to all topics.
//
// By default the aggregate channel closes only after every inner
// subscription has ended. With [SubWithTerminateOnInnerClose] (implied by
// [SubWithDisconnectOnLoss]) the first inner closure cancels the others and
// closes the aggregate channel.
func (b *SSEBroker) SubscribeMultiWith(topics []string, opts ...SubscribeOption) (msgs <-chan TopicMessage, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	cfg.rejectLossyCombinations()
	terminate := cfg.terminateOnInnerClose || cfg.disconnectOnLoss

	var innerOpts []SubscribeOption
	if cfg.scope != "" {
		innerOpts = append(innerOpts, SubWithScope(cfg.scope))
	}
	if cfg.filter != nil {
		innerOpts = append(innerOpts, SubWithFilter(cfg.filter))
	}
	if cfg.rate != nil {
		innerOpts = append(innerOpts, SubWithRate(*cfg.rate))
	}
	if cfg.meta != nil {
		innerOpts = append(innerOpts, SubWithMeta(*cfg.meta))
	}
	if cfg.snapshotFn != nil {
		innerOpts = append(innerOpts, SubWithSnapshot(cfg.snapshotFn))
	}
	if cfg.disconnectOnLoss {
		innerOpts = append(innerOpts, SubWithDisconnectOnLoss())
	}

	// Subscribe every topic before starting fan-in so the full unsubscribe
	// set exists before any worker can trigger termination.
	type inner struct {
		topic  string
		ch     <-chan string
		closed <-chan struct{} // nil unless terminate
	}
	inners := make([]inner, 0, len(topics))
	unsubs := make([]func(), 0, len(topics))
	for _, topic := range topics {
		ch, unsub := b.SubscribeWith(topic, innerOpts...)
		if ch == nil {
			if terminate {
				for _, u := range unsubs {
					u()
				}
				return nil, nil
			}
			continue
		}
		in := inner{topic: topic, ch: ch}
		if terminate {
			in.closed = b.closeSignal(topic, ch)
		}
		inners = append(inners, in)
		unsubs = append(unsubs, unsub)
	}

	out := make(chan TopicMessage, b.bufferSize)
	done := make(chan struct{})
	var wg sync.WaitGroup

	// doUnsub never waits for fan-in workers, so a worker may call it to
	// cancel its siblings.
	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			for _, unsub := range unsubs {
				unsub()
			}
		})
	}

	for _, in := range inners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-in.ch:
					if !ok {
						if terminate {
							doUnsub()
						}
						return
					}
					select {
					case out <- TopicMessage{Topic: in.topic, Data: msg}:
					case <-in.closed:
						// The inner subscription ended while the aggregate
						// was full; terminate without waiting for a reader.
						doUnsub()
						return
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	var closeOnce sync.Once
	closeOut := func() {
		closeOnce.Do(func() { close(out) })
	}

	go func() {
		wg.Wait()
		closeOut()
	}()

	return out, func() {
		doUnsub()
		wg.Wait()
		closeOut()
	}
}

// SubscribeGlobWith subscribes to a glob pattern with composable options.
// [SubWithDisconnectOnLoss] is not supported and panics;
// [SubWithTerminateOnInnerClose] has no effect.
func (b *SSEBroker) SubscribeGlobWith(pattern string, opts ...SubscribeOption) (msgs <-chan TopicMessage, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	if cfg.disconnectOnLoss {
		panic("tavern: SubscribeGlobWith does not support SubWithDisconnectOnLoss")
	}

	var rawCh <-chan TopicMessage
	var rawUnsub func()
	if cfg.scope != "" {
		rawCh, rawUnsub = b.SubscribeGlobScoped(pattern, cfg.scope)
	} else {
		rawCh, rawUnsub = b.SubscribeGlob(pattern)
	}

	if cfg.filter == nil && cfg.rate == nil {
		return rawCh, rawUnsub
	}

	out := make(chan TopicMessage, b.bufferSize)
	done := make(chan struct{})

	var rateState *globRateState
	if cfg.rate != nil {
		interval := cfg.rate.interval()
		if interval > 0 {
			rateState = &globRateState{interval: interval}
		}
	}

	go func() {
		defer close(out)
		for {
			select {
			case tm, ok := <-rawCh:
				if !ok {
					return
				}
				if cfg.filter != nil && !cfg.filter(tm.Data) {
					continue
				}
				if rateState != nil && !rateState.allow() {
					continue
				}
				select {
				case out <- tm:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()

	return out, func() {
		close(done)
		rawUnsub()
	}
}

// globRateState is a simple rate limiter for glob subscriptions.
type globRateState struct {
	interval time.Duration
	lastSent time.Time
}

func (s *globRateState) allow() bool {
	now := time.Now()
	if now.Sub(s.lastSent) < s.interval {
		return false
	}
	s.lastSent = now
	return true
}

// PublishWithTTL publishes a message with a TTL on the replay cache entry.
// Unlike other PublishBatch methods, this executes immediately rather than
// buffering because the TTL sweeper requires immediate processing.
func (pb *PublishBatch) PublishWithTTL(topic, msg string, ttl time.Duration, opts ...TTLOption) {
	// TTL publishes need immediate processing for the TTL sweeper, so we
	// execute them directly rather than buffering.
	pb.broker.PublishWithTTL(topic, msg, ttl, opts...)
}

// PublishWithID publishes a message with an associated event ID for
// Last-Event-ID resumption. Like [PublishBatch.PublishWithTTL], this executes
// immediately rather than buffering.
func (pb *PublishBatch) PublishWithID(topic, id, msg string) {
	pb.broker.PublishWithID(topic, id, msg)
}
