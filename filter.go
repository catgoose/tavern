package tavern

import (
	"sync/atomic"
	"time"
)

// FilterPredicate is a function that returns true if the message should be
// delivered to the subscriber. It runs synchronously in the publish goroutine
// for every message on the topic, so implementations must be fast and
// non-blocking. Messages rejected by the predicate are silently skipped and
// do not count toward drop counts or backpressure tiers.
type FilterPredicate func(msg string) bool

// SubscribeWithFilter registers a subscriber with a predicate filter for the
// given topic. Only messages for which the predicate returns true are delivered
// to the subscriber's channel. Non-matching messages are silently skipped and
// do not count toward drop counts or backpressure.
//
// The predicate runs in the publish goroutine — keep it fast.
func (b *SSEBroker) SubscribeWithFilter(topic string, predicate FilterPredicate) (msgs <-chan string, unsubscribe func()) {
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

// SubscribeScopedWithFilter registers a scoped subscriber with a predicate
// filter. Only messages published via [SSEBroker.PublishTo] with a matching
// scope AND passing the predicate are delivered. Non-matching messages are
// silently skipped.
func (b *SSEBroker) SubscribeScopedWithFilter(topic, scope string, predicate FilterPredicate) (msgs <-chan string, unsubscribe func()) {
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

// makeUnsubscribe returns an unsubscribe function for an unscoped subscriber.
func (b *SSEBroker) makeUnsubscribe(topic string, ch chan string) func() {
	return func() {
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

// makeScopedUnsubscribe returns an unsubscribe function for a scoped subscriber.
func (b *SSEBroker) makeScopedUnsubscribe(topic string, ch chan string) func() {
	return func() {
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
