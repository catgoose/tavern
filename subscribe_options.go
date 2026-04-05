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

	var ch <-chan string
	var unsub func()

	switch {
	case cfg.scope != "" && cfg.filter != nil:
		ch, unsub = b.SubscribeScopedWithFilter(topic, cfg.scope, cfg.filter)
	case cfg.scope != "":
		ch, unsub = b.SubscribeScoped(topic, cfg.scope)
	case cfg.filter != nil && cfg.snapshotFn != nil:
		// Filter + snapshot: use filter subscribe, then inject snapshot.
		ch, unsub = b.SubscribeWithFilter(topic, cfg.filter)
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

	// Inject snapshot for combos that didn't use SubscribeWithSnapshot.
	if cfg.snapshotFn != nil && (cfg.filter != nil || cfg.scope != "") {
		if snap := cfg.snapshotFn(); snap != "" {
			biCh := b.findBiChanAny(topic, ch)
			if biCh != nil {
				select {
				case biCh <- snap:
				default:
				}
			}
		}
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
func (b *SSEBroker) SubscribeMultiWith(topics []string, opts ...SubscribeOption) (msgs <-chan TopicMessage, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)

	out := make(chan TopicMessage, b.bufferSize)
	var unsubs []func()
	done := make(chan struct{})
	var wg sync.WaitGroup

	for _, topic := range topics {
		var innerOpts []SubscribeOption
		if cfg.filter != nil {
			innerOpts = append(innerOpts, SubWithFilter(cfg.filter))
		}
		if cfg.rate != nil {
			innerOpts = append(innerOpts, SubWithRate(*cfg.rate))
		}
		if cfg.meta != nil {
			innerOpts = append(innerOpts, SubWithMeta(*cfg.meta))
		}

		ch, unsub := b.SubscribeWith(topic, innerOpts...)
		if ch == nil {
			continue
		}
		unsubs = append(unsubs, unsub)
		t := topic
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- TopicMessage{Topic: t, Data: msg}:
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

	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			for _, unsub := range unsubs {
				unsub()
			}
		})
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
func (b *SSEBroker) SubscribeGlobWith(pattern string, opts ...SubscribeOption) (msgs <-chan TopicMessage, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)

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

// PublishWithTTLBatch buffers a TTL publish in the batch.
func (pb *PublishBatch) PublishWithTTL(topic, msg string, ttl time.Duration, opts ...TTLOption) {
	// TTL publishes need immediate processing for the TTL sweeper, so we
	// execute them directly rather than buffering.
	pb.broker.PublishWithTTL(topic, msg, ttl, opts...)
}

// PublishWithID buffers a publish with a replay ID in the batch.
func (pb *PublishBatch) PublishWithID(topic, id, msg string) {
	pb.broker.PublishWithID(topic, id, msg)
}
