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

	disconnectOnLoss       bool
	terminateOnInnerClose  bool
	terminateOnReplayError bool
	status                 *SubscriptionStatus
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

// SubWithStatus records how the subscription ends in status. See
// [SubscriptionStatus] for its guarantees. A status belongs to one
// subscription; attaching it twice panics.
//
// Supported by [SSEBroker.SubscribeWith], [SSEBroker.SubscribeFromIDWith]
// and [SSEBroker.SubscribeMultiWith]. For a multi-topic subscription status
// describes the aggregate: with [SubWithTerminateOnInnerClose] or
// [SubWithDisconnectOnLoss] it holds the cause and topic of the inner
// subscription that ended the aggregate; otherwise it holds the cause of the
// last inner subscription to end. [SSEBroker.SubscribeGlobWith] panics.
func SubWithStatus(status *SubscriptionStatus) SubscribeOption {
	return func(c *subscribeConfig) {
		c.status = status
	}
}

// SubWithTerminateOnReplayError ends the subscription with
// [TerminationReplayFailure] when a [ReplayStore] read for its replay
// fails, instead of continuing as if replay had succeeded (or, for a
// resume, as if the Last-Event-ID were not found). Nothing from the failed
// replay and no tavern-reconnected event is delivered; a snapshot already
// queued stays readable before the channel closes. The error is in the
// Termination recorded by [SubWithStatus] and is also reported to
// [WithReplayStoreErrorHandler].
//
// Without this option a failed read is reported but the subscription
// continues: Latest failures replay nothing and AfterID failures follow the
// Last-Event-ID-not-found gap policy. Subscriptions that do not read the
// store (filter or snapshot variants, or no ReplayStore) are unaffected.
// [SSEBroker.SubscribeGlobWith] panics.
func SubWithTerminateOnReplayError() SubscribeOption {
	return func(c *subscribeConfig) {
		c.terminateOnReplayError = true
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
	if cfg.status != nil {
		cfg.status.attach()
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

	// Loss-aware subscriptions replay like SubscribeFromIDWith with an empty
	// Last-Event-ID; see SubWithDisconnectOnLoss.
	if cfg.disconnectOnLoss {
		return b.subscribeFromIDInternal(topic, "", cfg)
	}

	spec := cfg.spec()
	if cfg.snapshotFn != nil {
		// Computed outside the broker lock in case snapshotFn uses the broker.
		spec.preload = cfg.snapshotFn()
	}
	return b.subscribeLive(topic, spec, cfg.snapshotFn != nil)
}

// spec converts the single-topic options into the registration policy.
// The snapshot is left to the caller, which decides when to compute it.
func (c subscribeConfig) spec() subSpec {
	spec := subSpec{
		scope:     c.scope,
		filter:    c.filter,
		meta:      c.meta,
		lossAware: c.disconnectOnLoss,
		status:    c.status,

		terminateOnReplayError: c.terminateOnReplayError,
	}
	if c.rate != nil {
		spec.rate = c.rate.interval()
	}
	return spec
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
	if cfg.terminateOnReplayError {
		innerOpts = append(innerOpts, SubWithTerminateOnReplayError())
	}
	status := cfg.status
	tracked := terminate || status != nil

	// Subscribe every topic before starting fan-in so the full unsubscribe
	// set exists before any worker can trigger termination.
	type inner struct {
		topic  string
		ch     <-chan string
		status *SubscriptionStatus // nil unless tracked
	}
	inners := make([]inner, 0, len(topics))
	unsubs := make([]func(), 0, len(topics))
	for _, topic := range topics {
		in := inner{topic: topic}
		opts := innerOpts
		if tracked {
			in.status = NewSubscriptionStatus()
			opts = append(innerOpts[:len(innerOpts):len(innerOpts)], SubWithStatus(in.status))
		}
		ch, unsub := b.SubscribeWith(topic, opts...)
		if ch == nil {
			if terminate {
				status.finish(Termination{Reason: TerminationRejected, Topic: topic})
				for _, u := range unsubs {
					u()
				}
				return nil, nil
			}
			continue
		}
		in.ch = ch
		inners = append(inners, in)
		unsubs = append(unsubs, unsub)
	}

	innerEnded := func(in inner) {
		if terminate {
			status.finish(in.status.Termination())
		}
	}
	// innerDone is nil (never ready) unless terminate.
	innerDone := func(in inner) <-chan struct{} {
		if terminate {
			return in.status.Done()
		}
		return nil
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
						innerEnded(in)
						if terminate {
							doUnsub()
						}
						return
					}
					select {
					case out <- TopicMessage{Topic: in.topic, Data: msg}:
					case <-innerDone(in):
						// The inner subscription ended while the aggregate
						// was full; terminate without waiting for a reader.
						innerEnded(in)
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
		if status == nil {
			closeOut()
			return
		}
		// Without terminate the aggregate ends with its last inner
		// subscription.
		innerStatuses := make([]*SubscriptionStatus, len(inners))
		for i, in := range inners {
			innerStatuses[i] = in.status
		}
		if t, ok := lastEnded(innerStatuses); ok {
			status.finish(t)
		} else {
			// No inner subscription was admitted.
			status.finish(Termination{Reason: TerminationRejected})
		}
		closeOut()
	}()

	return out, func() {
		status.finish(Termination{Reason: TerminationUnsubscribe})
		doUnsub()
		wg.Wait()
		closeOut()
	}
}

// SubscribeGlobWith subscribes to a glob pattern with composable options.
// [SubWithDisconnectOnLoss], [SubWithStatus] and
// [SubWithTerminateOnReplayError] are not supported and panic;
// [SubWithTerminateOnInnerClose] has no effect.
func (b *SSEBroker) SubscribeGlobWith(pattern string, opts ...SubscribeOption) (msgs <-chan TopicMessage, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	if cfg.disconnectOnLoss {
		panic("tavern: SubscribeGlobWith does not support SubWithDisconnectOnLoss")
	}
	if cfg.status != nil {
		panic("tavern: SubscribeGlobWith does not support SubWithStatus")
	}
	if cfg.terminateOnReplayError {
		panic("tavern: SubscribeGlobWith does not support SubWithTerminateOnReplayError")
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

	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(done)
			rawUnsub()
		})
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
