package tavern

import (
	"sync"
	"time"
)

// Rate configures per-subscriber rate limiting. If both MaxPerSecond and
// MinInterval are set, MinInterval takes precedence.
type Rate struct {
	// MaxPerSecond is a convenience field: converted to MinInterval internally.
	MaxPerSecond float64
	// MinInterval is the minimum time between deliveries to this subscriber.
	MinInterval time.Duration
}

// interval returns the effective minimum interval, converting MaxPerSecond
// if MinInterval is not set.
func (r Rate) interval() time.Duration {
	if r.MinInterval > 0 {
		return r.MinInterval
	}
	if r.MaxPerSecond > 0 {
		return time.Duration(float64(time.Second) / r.MaxPerSecond)
	}
	return 0
}

// rateLimitedSub tracks state for a single rate-limited subscriber.
type rateLimitedSub struct {
	ch          chan string
	topic       string
	minInterval time.Duration
	lastSent    time.Time
	pending     *string // latest held message (nil = nothing pending)
}

// rateLimiter manages per-subscriber rate limiting with a single shared ticker.
type rateLimiter struct {
	mu      sync.Mutex
	subs    map[chan string]*rateLimitedSub
	ticker  *time.Ticker
	done    chan struct{}
	running bool
}

const rateTickInterval = 10 * time.Millisecond

// newRateLimiter creates an idle rate limiter. The shared ticker is started
// lazily when the first rate-limited subscriber is added.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		subs: make(map[chan string]*rateLimitedSub),
		done: make(chan struct{}),
	}
}

// add registers a rate-limited subscriber. Starts the shared ticker if this
// is the first subscriber.
func (rl *rateLimiter) add(ch chan string, topic string, interval time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.subs[ch] = &rateLimitedSub{
		ch:          ch,
		topic:       topic,
		minInterval: interval,
	}
	if !rl.running {
		rl.running = true
		rl.ticker = time.NewTicker(rateTickInterval)
		go rl.loop()
	}
}

// remove unregisters a rate-limited subscriber. Stops the ticker if no
// subscribers remain.
func (rl *rateLimiter) remove(ch chan string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.subs, ch)
	if len(rl.subs) == 0 && rl.running {
		rl.ticker.Stop()
		rl.running = false
	}
}

// hold stores a pending message for the subscriber. Returns true if the
// subscriber is rate-limited and the message was held (caller should skip
// the normal send). Returns false if the subscriber is not rate-limited or
// enough time has elapsed for immediate delivery.
func (rl *rateLimiter) hold(ch chan string, msg string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	sub, ok := rl.subs[ch]
	if !ok {
		return false
	}
	now := time.Now()
	if now.Sub(sub.lastSent) >= sub.minInterval {
		// Enough time passed — allow immediate send and update timestamp.
		sub.lastSent = now
		return false
	}
	// Within interval — hold latest message.
	sub.pending = &msg
	return true
}

// loop is the shared ticker goroutine. It checks all rate-limited subscribers
// and flushes pending messages whose interval has elapsed.
func (rl *rateLimiter) loop() {
	for {
		select {
		case <-rl.done:
			return
		case now := <-rl.ticker.C:
			rl.flush(now)
		}
	}
}

// flush sends any pending messages whose interval has elapsed.
func (rl *rateLimiter) flush(now time.Time) {
	rl.mu.Lock()
	// Collect channels that need flushing while holding the lock.
	type pending struct {
		ch  chan string
		msg string
	}
	var toSend []pending
	for _, sub := range rl.subs {
		if sub.pending == nil {
			continue
		}
		if now.Sub(sub.lastSent) >= sub.minInterval {
			toSend = append(toSend, pending{ch: sub.ch, msg: *sub.pending})
			sub.pending = nil
			sub.lastSent = now
		}
	}
	rl.mu.Unlock()

	// Send outside the lock to avoid blocking.
	for _, p := range toSend {
		func() {
			defer func() { _ = recover() }()
			select {
			case p.ch <- p.msg:
			default:
			}
		}()
	}
}

// stop shuts down the shared ticker goroutine and clears all state.
func (rl *rateLimiter) stop() {
	close(rl.done)
	rl.mu.Lock()
	if rl.running {
		rl.ticker.Stop()
		rl.running = false
	}
	rl.subs = nil
	rl.mu.Unlock()
}

// SubscribeWithRate registers a subscriber with per-subscriber rate limiting.
// Messages published faster than the configured rate are held, and the most
// recent held message is delivered when the interval elapses (latest-wins).
// Rate limiting is per-subscriber and does not affect the publisher or other
// subscribers.
func (b *SSEBroker) SubscribeWithRate(topic string, rate Rate) (msgs <-chan string, unsubscribe func()) {
	ch, unsub := b.Subscribe(topic)
	interval := rate.interval()
	if interval <= 0 {
		return ch, unsub
	}

	// The bidirectional channel is stored in topics[topic]; we need it to
	// register with the rate limiter. Recover it via the read-only channel.
	biCh := b.findBiChan(topic, ch)
	if biCh == nil {
		return ch, unsub
	}

	b.rateLimit.add(biCh, topic, interval)
	return ch, func() {
		b.rateLimit.remove(biCh)
		unsub()
	}
}

// SubscribeScopedWithRate registers a scoped subscriber with per-subscriber
// rate limiting. See [SSEBroker.SubscribeWithRate] for rate-limiting semantics.
func (b *SSEBroker) SubscribeScopedWithRate(topic, scope string, rate Rate) (msgs <-chan string, unsubscribe func()) {
	ch, unsub := b.SubscribeScoped(topic, scope)
	interval := rate.interval()
	if interval <= 0 {
		return ch, unsub
	}

	biCh := b.findBiScopedChan(topic, ch)
	if biCh == nil {
		return ch, unsub
	}

	b.rateLimit.add(biCh, topic, interval)
	return ch, func() {
		b.rateLimit.remove(biCh)
		unsub()
	}
}

// findBiChan recovers the bidirectional channel from the topics map by
// comparing it to the read-only channel returned by Subscribe.
func (b *SSEBroker) findBiChan(topic string, readOnly <-chan string) chan string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for c := range b.topics[topic] {
		if (<-chan string)(c) == readOnly { //nolint:gocritic // parens required for channel type conversion
			return c
		}
	}
	return nil
}

// findBiScopedChan recovers the bidirectional channel from the scopedTopics map.
func (b *SSEBroker) findBiScopedChan(topic string, readOnly <-chan string) chan string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for c := range b.scopedTopics[topic] {
		if (<-chan string)(c) == readOnly { //nolint:gocritic // parens required for channel type conversion
			return c
		}
	}
	return nil
}
