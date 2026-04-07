package tavern

import (
	"sync/atomic"
	"time"
)

// SubscribeWithSnapshot subscribes to a topic and immediately sends the
// result of snapshotFn as the first message before any live publishes.
// The snapshot function runs while the subscription is being registered,
// ensuring no messages are missed between the snapshot and live stream.
// If snapshotFn returns an empty string, no snapshot is sent.
//
// This eliminates the dual-render pattern where page handlers and publishers
// independently render the same initial state.
func (b *SSEBroker) SubscribeWithSnapshot(topic string, snapshotFn func() string) (msgs <-chan string, unsubscribe func()) {
	// Compute snapshot outside lock to avoid deadlock if snapshotFn accesses broker.
	snap := snapshotFn()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if !b.admitSubscriber(topic) {
		b.mu.Unlock()
		return nil, nil
	}
	ch := make(chan string, b.bufferSize)

	// Inject snapshot BEFORE registering — no live message can arrive first.
	if snap != "" {
		select {
		case ch <- snap:
		default:
		}
	}

	if b.topics[topic] == nil {
		b.topics[topic] = make(map[chan string]struct{})
	}
	b.topics[topic][ch] = struct{}{}
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
	delete(b.topicEmpty, topic)
	b.mu.Unlock()

	for _, fn := range firstHooks {
		go fn(topic)
	}

	return ch, func() {
		b.mu.Lock()
		var lastHooks []func(string)
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
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
	}
}

// subscribeScopedWithSnapshot creates a scoped subscription with an atomic
// pre-registration snapshot.
func (b *SSEBroker) subscribeScopedWithSnapshot(topic, scope, snap string) (<-chan string, func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if !b.admitSubscriber(topic) {
		b.mu.Unlock()
		return nil, nil
	}
	ch := make(chan string, b.bufferSize)
	if snap != "" {
		select {
		case ch <- snap:
		default:
		}
	}
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
	if b.evictThreshold > 0 || b.adaptive != nil {
		b.dropCountsMu.Lock()
		b.dropCounts[ch] = &atomic.Int64{}
		b.dropCountsMu.Unlock()
	}
	if b.adaptive != nil {
		b.adaptive.getState(ch)
	}
	delete(b.topicEmpty, topic)
	hasBackend := b.backend != nil
	b.mu.Unlock()
	for _, fn := range firstHooks {
		go fn(topic)
	}
	if hasBackend && total == 1 {
		b.backendSubscribe(topic)
	}
	b.publishConnectionEvent("subscribe", topic, total)
	return ch, b.makeScopedUnsubscribe(topic, ch)
}

// subscribeWithFilterAndSnapshot creates a filtered subscription with an
// atomic pre-registration snapshot.
func (b *SSEBroker) subscribeWithFilterAndSnapshot(topic string, predicate FilterPredicate, snap string) (<-chan string, func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if !b.admitSubscriber(topic) {
		b.mu.Unlock()
		return nil, nil
	}
	ch := make(chan string, b.bufferSize)
	if snap != "" {
		select {
		case ch <- snap:
		default:
		}
	}
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[chan string]struct{})
	}
	b.topics[topic][ch] = struct{}{}
	b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, ConnectedAt: time.Now()}
	if b.filterPredicates == nil {
		b.filterPredicates = make(map[chan string]FilterPredicate)
	}
	b.filterPredicates[ch] = predicate
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
		if predicate != nil && !predicate(msg) {
			continue
		}
		select {
		case ch <- msg:
		default:
		}
	}
	return ch, b.makeUnsubscribe(topic, ch)
}

// subscribeScopedWithFilterAndSnapshot creates a scoped+filtered subscription
// with an atomic pre-registration snapshot.
func (b *SSEBroker) subscribeScopedWithFilterAndSnapshot(topic, scope string, predicate FilterPredicate, snap string) (<-chan string, func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if !b.admitSubscriber(topic) {
		b.mu.Unlock()
		return nil, nil
	}
	ch := make(chan string, b.bufferSize)
	if snap != "" {
		select {
		case ch <- snap:
		default:
		}
	}
	if b.scopedTopics[topic] == nil {
		b.scopedTopics[topic] = make(map[chan string]scopedSub)
	}
	b.scopedTopics[topic][ch] = scopedSub{scope: scope}
	b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, Scope: scope, ConnectedAt: time.Now()}
	if b.filterPredicates == nil {
		b.filterPredicates = make(map[chan string]FilterPredicate)
	}
	b.filterPredicates[ch] = predicate
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
		if predicate != nil && !predicate(msg) {
			continue
		}
		select {
		case ch <- msg:
		default:
		}
	}
	return ch, b.makeScopedUnsubscribe(topic, ch)
}
