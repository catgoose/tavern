package tavern

import (
	"sync"
	"sync/atomic"
	"time"
)

// coalescingState holds the atomic pointer and signal channel used by a
// coalescing subscriber. Instead of buffering every published message, only
// the latest value is kept; the reader goroutine delivers it when signaled.
type coalescingState struct {
	latest atomic.Pointer[string]
	signal chan struct{} // buffered 1
}

// SubscribeWithCoalescing registers a subscriber that automatically coalesces
// rapid updates. When multiple messages are published before the subscriber
// reads, only the latest value is delivered. This is ideal for high-frequency
// data like stock tickers or sensor readings where intermediate values are
// irrelevant.
//
// Replaced (coalesced) messages do not count as drops in [SSEBroker.PublishDrops].
// The coalescing channel uses an internal atomic pointer so updates are
// lock-free in the publish path.
//
// The returned channel receives the latest message whenever a new value is
// available. Call the returned function to unsubscribe and close the channel.
// The unsubscribe function is safe to call more than once.
func (b *SSEBroker) SubscribeWithCoalescing(topic string) (msgs <-chan string, unsubscribe func()) {
	return b.subscribeCoalescing(topic, "")
}

// SubscribeScopedWithCoalescing registers a scoped coalescing subscriber.
// Only messages published via [SSEBroker.PublishTo] with a matching scope are
// delivered, and rapid updates are coalesced to the latest value.
func (b *SSEBroker) SubscribeScopedWithCoalescing(topic, scope string) (msgs <-chan string, unsubscribe func()) {
	return b.subscribeCoalescing(topic, scope)
}

func (b *SSEBroker) subscribeCoalescing(topic, scope string) (msgs <-chan string, unsubscribe func()) {
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

	// The internal channel participates in the normal fan-out path but is
	// intercepted by publishToChannels when coalescing is enabled.
	internalCh := make(chan string, b.bufferSize)
	cs := &coalescingState{
		signal: make(chan struct{}, 1),
	}

	if scope == "" {
		if b.topics[topic] == nil {
			b.topics[topic] = make(map[chan string]struct{})
		}
		b.topics[topic][internalCh] = struct{}{}
	} else {
		if b.scopedTopics[topic] == nil {
			b.scopedTopics[topic] = make(map[chan string]scopedSub)
		}
		b.scopedTopics[topic][internalCh] = scopedSub{scope: scope}
	}

	b.subscriberMeta[internalCh] = &SubscriberInfo{Topic: topic, Scope: scope, ConnectedAt: time.Now()}

	if b.coalescingChans == nil {
		b.coalescingChans = make(map[chan string]*coalescingState)
	}
	b.coalescingChans[internalCh] = cs

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
	hasBackend := b.backend != nil
	delete(b.topicEmpty, topic)
	b.mu.Unlock()

	for _, fn := range firstHooks {
		go fn(topic)
	}
	if hasBackend && total == 1 {
		b.backendSubscribe(topic)
	}
	b.publishConnectionEvent("subscribe", topic, total)

	// The public channel is fed by a goroutine that reads signals and loads
	// the latest value from the atomic pointer.
	out := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		defer close(out)
		for {
			select {
			case <-cs.signal:
				ptr := cs.latest.Load()
				if ptr == nil {
					continue
				}
				select {
				case out <- *ptr:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	connectedAt := time.Now()

	return out, func() {
		once.Do(func() {
			close(done)

			b.mu.Lock()
			var lastHooks []func(string)
			var isLast bool

			if scope == "" {
				if _, ok := b.topics[topic][internalCh]; ok {
					delete(b.topics[topic], internalCh)
					total := len(b.topics[topic]) + len(b.scopedTopics[topic])
					if total == 0 {
						isLast = true
						lastHooks = b.onLast[topic]
						b.topicEmpty[topic] = time.Now()
					}
				}
			} else {
				if _, ok := b.scopedTopics[topic][internalCh]; ok {
					delete(b.scopedTopics[topic], internalCh)
					total := len(b.topics[topic]) + len(b.scopedTopics[topic])
					if total == 0 {
						isLast = true
						lastHooks = b.onLast[topic]
						b.topicEmpty[topic] = time.Now()
					}
				}
			}
			delete(b.subscriberMeta, internalCh)
			delete(b.filterPredicates, internalCh)
			delete(b.coalescingChans, internalCh)
			close(internalCh)
			b.mu.Unlock()

			if b.evictThreshold > 0 || b.adaptive != nil {
				b.dropCountsMu.Lock()
				delete(b.dropCounts, internalCh)
				b.dropCountsMu.Unlock()
			}
			if b.obs != nil {
				b.obs.recordConnectionDuration(topic, time.Since(connectedAt))
			}
			for _, fn := range lastHooks {
				go fn(topic)
			}
			if hasBackend && isLast {
				b.backendUnsubscribe(topic)
			}
			b.publishConnectionEvent("unsubscribe", topic, -1)
		})
	}
}

// coalesceStore atomically stores the message for a coalescing channel and
// signals the reader goroutine. Returns true if the channel is coalescing
// (message was stored), false otherwise.
func (b *SSEBroker) coalesceStore(ch chan string, msg string) bool {
	b.mu.RLock()
	cs, ok := b.coalescingChans[ch]
	b.mu.RUnlock()
	if !ok {
		return false
	}
	cs.latest.Store(&msg)
	select {
	case cs.signal <- struct{}{}:
	default:
		// Already signaled — reader will pick up the latest value.
	}
	return true
}
