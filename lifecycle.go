package tavern

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern/backend"
)

// subSpec is the delivery policy of a single-topic subscriber. registerLocked
// installs all of it in the critical section that makes the channel visible
// to publishers, so no publish can observe a partially configured
// subscriber.
type subSpec struct {
	scope      string
	filter     FilterPredicate
	meta       *SubscribeMeta
	rate       time.Duration // 0 = not rate-limited
	lossAware  bool
	coalescing *coalescingState
	// preload is queued before the channel becomes visible, so it is always
	// the first message (snapshot-first delivery). Empty = nothing.
	preload string
}

type joined struct {
	ch         chan string
	total      int
	firstHooks []func(string)
	// closing is set only for coalescing subscribers; their output worker
	// exits when ch starts closing.
	closing <-chan struct{}
}

type left struct {
	removed     bool
	isLast      bool
	lastHooks   []func(string)
	connectedAt time.Time
}

// rejectedSubscription is returned when registration does not happen: a
// closed channel and a no-op unsubscribe after Close, or nil, nil when
// admission control rejects the subscriber.
func rejectedSubscription(closed bool) (msgs <-chan string, unsubscribe func()) {
	if !closed {
		return nil, nil
	}
	ch := make(chan string)
	close(ch)
	return ch, func() {}
}

// registerLocked creates and registers a subscriber channel for topic with
// the given policy. The caller holds b.mu and runs finishJoin after
// unlocking. It returns nil when the broker is closed (closed is true) or
// admission control rejects the subscriber.
func (b *SSEBroker) registerLocked(topic string, spec subSpec) (j *joined, closed bool) {
	if b.closed {
		return nil, true
	}
	if !b.admitSubscriber(topic) {
		return nil, false
	}
	ch := make(chan string, b.bufferSize)
	g := &chanGuard{lossAware: spec.lossAware}
	j = &joined{ch: ch}
	if spec.coalescing != nil {
		j.closing = g.signal()
	}
	b.chanGuards.Store(ch, g)
	if spec.preload != "" {
		select {
		case ch <- spec.preload:
		default:
		}
	}

	info := &SubscriberInfo{Topic: topic, Scope: spec.scope, ConnectedAt: time.Now()}
	if spec.meta != nil {
		info.ID = spec.meta.ID
		info.Meta = spec.meta.Meta
	}
	b.subscriberMeta[ch] = info
	if spec.filter != nil {
		if b.filterPredicates == nil {
			b.filterPredicates = make(map[chan string]FilterPredicate)
		}
		b.filterPredicates[ch] = spec.filter
	}
	if spec.coalescing != nil {
		if b.coalescingChans == nil {
			b.coalescingChans = make(map[chan string]*coalescingState)
		}
		b.coalescingChans[ch] = spec.coalescing
	}
	if b.evictThreshold > 0 || b.adaptive != nil {
		b.dropCountsMu.Lock()
		b.dropCounts[ch] = &atomic.Int64{}
		b.dropCountsMu.Unlock()
	}
	if b.adaptive != nil {
		b.adaptive.getState(ch)
	}
	if spec.rate > 0 {
		b.rateLimit.add(ch, topic, spec.rate)
	}

	if spec.scope != "" {
		if b.scopedTopics[topic] == nil {
			b.scopedTopics[topic] = make(map[chan string]scopedSub)
		}
		b.scopedTopics[topic][ch] = scopedSub{scope: spec.scope}
	} else {
		if b.topics[topic] == nil {
			b.topics[topic] = make(map[chan string]struct{})
		}
		b.topics[topic][ch] = struct{}{}
	}
	j.total = len(b.topics[topic]) + len(b.scopedTopics[topic])
	if j.total == 1 {
		j.firstHooks = b.onFirst[topic]
	}
	if b.metrics != nil && j.total > b.metrics.peakSubs[topic] {
		b.metrics.peakSubs[topic] = j.total
	}
	delete(b.topicEmpty, topic)
	return j, false
}

// finishJoin runs the parts of registration that must not hold b.mu: first
// subscriber hooks, backend registration and the connection event.
func (b *SSEBroker) finishJoin(topic string, j *joined) {
	for _, fn := range j.firstHooks {
		go fn(topic)
	}
	if j.total == 1 {
		b.syncBackend(topic)
	}
	b.publishConnectionEvent("subscribe", topic, j.total)
}

// removeLocked unregisters ch from topic, releases its delivery policy and
// closes it. The caller holds b.mu and runs finishLeave after unlocking. A
// subscriber that is already gone is left alone (removed is false).
func (b *SSEBroker) removeLocked(topic string, ch chan string) left {
	_, unscoped := b.topics[topic][ch]
	_, scoped := b.scopedTopics[topic][ch]
	if !unscoped && !scoped {
		return left{}
	}
	l := left{removed: true}
	if info := b.subscriberMeta[ch]; info != nil {
		l.connectedAt = info.ConnectedAt
	}
	delete(b.topics[topic], ch)
	delete(b.scopedTopics[topic], ch)
	delete(b.subscriberMeta, ch)
	delete(b.filterPredicates, ch)
	delete(b.coalescingChans, ch)
	b.rateLimit.remove(ch)
	b.closeChan(ch)
	if len(b.topics[topic])+len(b.scopedTopics[topic]) == 0 {
		l.isLast = true
		l.lastHooks = b.onLast[topic]
		b.topicEmpty[topic] = time.Now()
	}
	return l
}

// finishLeave releases per-subscriber state kept outside b.mu and runs the
// last-subscriber transition. It is safe to call when removeLocked found
// nothing to remove.
func (b *SSEBroker) finishLeave(topic string, ch chan string, l left) {
	b.dropCountsMu.Lock()
	delete(b.dropCounts, ch)
	b.dropCountsMu.Unlock()
	if b.adaptive != nil {
		b.adaptive.removeState(ch)
	}
	if !l.removed {
		return
	}
	if b.obs != nil && !l.connectedAt.IsZero() {
		b.obs.recordConnectionDuration(topic, time.Since(l.connectedAt))
	}
	for _, fn := range l.lastHooks {
		go fn(topic)
	}
	if l.isLast {
		b.syncBackend(topic)
	}
}

func (b *SSEBroker) removeSubscriber(topic string, ch chan string) bool {
	b.mu.Lock()
	l := b.removeLocked(topic, ch)
	b.mu.Unlock()
	b.finishLeave(topic, ch, l)
	return l.removed
}

// unsubscriber returns the unsubscribe function for a registered subscriber.
// Calling it after any other removal path, or more than once, is a no-op.
func (b *SSEBroker) unsubscriber(topic string, ch chan string) func() {
	return func() {
		if b.removeSubscriber(topic, ch) {
			b.publishConnectionEvent("unsubscribe", topic, -1)
		}
	}
}

// backendTopic tracks the broker's backend subscription for one topic. mu
// serializes subscribe and unsubscribe calls so that first and last
// subscriber transitions racing each other converge on the current local
// state instead of applying in whatever order they reach the backend.
type backendTopic struct {
	mu     sync.Mutex
	active bool
	ch     <-chan backend.MessageEnvelope // channel being fanned in
	refs   int                            // guarded by SSEBroker.backendMu
}

func (b *SSEBroker) acquireBackendTopic(topic string) *backendTopic {
	b.backendMu.Lock()
	defer b.backendMu.Unlock()
	if b.backendTopics == nil {
		b.backendTopics = make(map[string]*backendTopic)
	}
	st := b.backendTopics[topic]
	if st == nil {
		st = &backendTopic{}
		b.backendTopics[topic] = st
	}
	st.refs++
	return st
}

// releaseBackendTopic drops the entry once nobody uses it and it holds no
// backend subscription, keeping the map bounded by active topics.
func (b *SSEBroker) releaseBackendTopic(topic string, st *backendTopic) {
	b.backendMu.Lock()
	defer b.backendMu.Unlock()
	st.refs--
	if st.refs == 0 && !st.active {
		delete(b.backendTopics, topic)
	}
}

// localSubscribers reports whether topic has local subscribers on an open
// broker.
func (b *SSEBroker) localSubscribers(topic string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return !b.closed && len(b.topics[topic])+len(b.scopedTopics[topic]) > 0
}

// syncBackend makes the backend subscription for topic match whether the
// topic has local subscribers. It is called after first and last subscriber
// transitions without holding b.mu, so backend calls never block publishers.
func (b *SSEBroker) syncBackend(topic string) {
	if b.backend == nil {
		return
	}
	st := b.acquireBackendTopic(topic)
	defer b.releaseBackendTopic(topic, st)
	st.mu.Lock()
	defer st.mu.Unlock()
	want := b.localSubscribers(topic)
	switch {
	case want && !st.active:
		st.active = b.backendSubscribe(topic, st)
	case !want && st.active:
		b.backendUnsubscribe(topic)
		st.active = false
		st.ch = nil
	}
}
