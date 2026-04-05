package tavern

import (
	"errors"
	"time"
)

// ErrPublishTimeout is returned by [SSEBroker.PublishBlocking] and
// [SSEBroker.PublishBlockingTo] when at least one subscriber's channel could
// not accept the message within the configured timeout.
var ErrPublishTimeout = errors.New("tavern: publish timeout")

// OnPublishDrop registers a callback that fires each time a message is
// dropped during fan-out because a subscriber's channel buffer is full.
// The callback receives the topic name and the number of subscribers for
// whom delivery failed. Only one callback is supported; subsequent calls
// replace the previous one.
//
// The callback runs synchronously in the publish goroutine — keep it fast.
func (b *SSEBroker) OnPublishDrop(fn func(topic string, droppedForCount int)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onPublishDropFn = fn
}

// PublishBlocking behaves like [SSEBroker.Publish] but blocks up to timeout
// for each subscriber whose channel buffer is full. If any subscriber cannot
// accept the message within the timeout, [ErrPublishTimeout] is returned.
// A timeout of zero falls back to non-blocking behavior (equivalent to
// [SSEBroker.Publish]).
func (b *SSEBroker) PublishBlocking(topic, msg string, timeout time.Duration) error {
	if timeout <= 0 {
		b.Publish(topic, msg)
		return nil
	}
	return b.publishBlocking(topic, "", msg, timeout)
}

// PublishBlockingTo behaves like [SSEBroker.PublishTo] but blocks up to
// timeout for each matching scoped subscriber whose channel buffer is full.
// Returns [ErrPublishTimeout] if any delivery times out.
func (b *SSEBroker) PublishBlockingTo(topic, scope, msg string, timeout time.Duration) error {
	if timeout <= 0 {
		b.PublishTo(topic, scope, msg)
		return nil
	}
	return b.publishBlocking(topic, scope, msg, timeout)
}

func (b *SSEBroker) publishBlocking(topic, scope, msg string, timeout time.Duration) error {
	unlock := b.orderLock(topic)
	defer unlock()

	b.mu.RLock()
	var channels []chan string
	if scope == "" {
		subscribers := b.topics[topic]
		channels = make([]chan string, 0, len(subscribers))
		for ch := range subscribers {
			channels = append(channels, ch)
		}
	} else {
		scopedSubs := b.scopedTopics[topic]
		channels = make([]chan string, 0, len(scopedSubs))
		for ch, sub := range scopedSubs {
			if sub.scope == scope {
				channels = append(channels, ch)
			}
		}
	}
	b.mu.RUnlock()

	var timedOut bool
	for _, ch := range channels {
		func() {
			defer func() { _ = recover() }()

			// Check filter predicate.
			if pred := b.filterPredicate(ch); pred != nil && !pred(msg) {
				return
			}

			// Coalescing bypass.
			if b.coalesceStore(ch, msg) {
				return
			}

			select {
			case ch <- msg:
			case <-time.After(timeout):
				timedOut = true
			}
		}()
	}

	b.dispatchToGlobSubscribers(topic, scope, msg)
	b.backendPublish(topic, msg, scope)
	b.fireAfterHooks(topic)

	if timedOut {
		return ErrPublishTimeout
	}
	return nil
}
