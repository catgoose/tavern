package tavern

import (
	"encoding/json"
	"fmt"
	"sync"
)

// topicsChangedEvent is the SSE event name sent to clients when their
// subscription set changes via [SSEBroker.AddTopic] or [SSEBroker.RemoveTopic].
const topicsChangedEvent = "tavern-topics-changed"

// multiSub tracks a multiplexed subscriber that was created via
// [SSEBroker.SubscribeMulti] or upgraded from a single-topic subscriber
// via [SSEBroker.AddTopic].
type multiSub struct {
	id     string
	out    chan TopicMessage
	done   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	topics map[string]chan string // topic → internal channel
	unsubs map[string]func()     // topic → unsubscribe function
}

// AddTopic adds a topic to an existing subscriber identified by subscriberID.
// The subscriber starts receiving messages from the new topic without
// reconnecting. If the subscriber is already subscribed to the topic, this is
// a no-op and returns false. Returns true if the topic was successfully added.
//
// If sendControl is true, a control event with type "tavern-topics-changed" is
// sent on the subscriber's channel so the client can react (e.g., set up new
// SSE-swap targets).
func (b *SSEBroker) AddTopic(subscriberID, topic string, sendControl bool) bool {
	b.mu.RLock()
	ms := b.findMultiSub(subscriberID)
	b.mu.RUnlock()

	if ms == nil {
		return false
	}

	ms.mu.Lock()
	if _, already := ms.topics[topic]; already {
		ms.mu.Unlock()
		return false
	}
	ms.mu.Unlock()

	ch, unsub := b.Subscribe(topic)

	// Attach subscriber metadata (ID) to the new channel.
	b.mu.Lock()
	bidir := b.findBidirChan(topic, ch)
	if bidir != nil {
		if info, ok := b.subscriberMeta[bidir]; ok {
			info.ID = subscriberID
		}
	}
	b.mu.Unlock()

	ms.mu.Lock()
	// Double-check after re-acquiring lock.
	if _, already := ms.topics[topic]; already {
		ms.mu.Unlock()
		unsub()
		return false
	}
	ms.topics[topic] = b.readToBidir(topic, ch)
	ms.unsubs[topic] = unsub
	ms.mu.Unlock()

	// Start fan-in goroutine for the new topic.
	ms.wg.Add(1)
	go func() {
		defer ms.wg.Done()
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				select {
				case ms.out <- TopicMessage{Topic: topic, Data: msg}:
				case <-ms.done:
					return
				}
			case <-ms.done:
				return
			}
		}
	}()

	if sendControl {
		b.sendTopicsChangedEvent(ms, "added", topic)
	}

	return true
}

// RemoveTopic removes a topic from an existing subscriber identified by
// subscriberID. The subscriber stops receiving messages from the topic.
// Returns true if the topic was found and removed. Lifecycle hooks
// (OnLastUnsubscribe) fire if this was the last subscriber on the topic.
//
// If sendControl is true, a control event with type "tavern-topics-changed" is
// sent on the subscriber's channel.
func (b *SSEBroker) RemoveTopic(subscriberID, topic string, sendControl bool) bool {
	b.mu.RLock()
	ms := b.findMultiSub(subscriberID)
	b.mu.RUnlock()

	if ms == nil {
		return false
	}

	ms.mu.Lock()
	unsub, ok := ms.unsubs[topic]
	if !ok {
		ms.mu.Unlock()
		return false
	}
	delete(ms.topics, topic)
	delete(ms.unsubs, topic)
	ms.mu.Unlock()

	unsub()

	if sendControl {
		b.sendTopicsChangedEvent(ms, "removed", topic)
	}

	return true
}

// AddTopicForScope adds a topic to all subscribers with the matching scope.
// Returns the number of subscribers that had the topic added.
func (b *SSEBroker) AddTopicForScope(scope, topic string, sendControl bool) int {
	subs := b.findMultiSubsByScope(scope)
	count := 0
	for _, ms := range subs {
		if b.AddTopic(ms.id, topic, sendControl) {
			count++
		}
	}
	return count
}

// RemoveTopicForScope removes a topic from all subscribers with the matching
// scope. Returns the number of subscribers that had the topic removed.
func (b *SSEBroker) RemoveTopicForScope(scope, topic string, sendControl bool) int {
	subs := b.findMultiSubsByScope(scope)
	count := 0
	for _, ms := range subs {
		if b.RemoveTopic(ms.id, topic, sendControl) {
			count++
		}
	}
	return count
}

// SubscribeMultiWithMeta subscribes to multiple topics with metadata and
// returns a managed multi-subscription that supports dynamic topic changes
// via [SSEBroker.AddTopic] and [SSEBroker.RemoveTopic].
func (b *SSEBroker) SubscribeMultiWithMeta(meta SubscribeMeta, topics ...string) (msgs <-chan TopicMessage, unsubscribe func()) {
	out := make(chan TopicMessage, b.bufferSize)
	done := make(chan struct{})

	ms := &multiSub{
		id:     meta.ID,
		out:    out,
		done:   done,
		topics: make(map[string]chan string, len(topics)),
		unsubs: make(map[string]func(), len(topics)),
	}

	for _, topic := range topics {
		ch, unsub := b.Subscribe(topic)
		bidir := b.readToBidir(topic, ch)

		// Attach subscriber metadata.
		b.mu.Lock()
		if bidir != nil {
			if info, ok := b.subscriberMeta[bidir]; ok {
				info.ID = meta.ID
				info.Meta = meta.Meta
			}
		}
		b.mu.Unlock()

		ms.topics[topic] = bidir
		ms.unsubs[topic] = unsub

		t := topic
		ms.wg.Add(1)
		go func() {
			defer ms.wg.Done()
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

	// Register the multi-sub for lookup.
	b.mu.Lock()
	if b.multiSubs == nil {
		b.multiSubs = make(map[string]*multiSub)
	}
	b.multiSubs[meta.ID] = ms
	b.mu.Unlock()

	var closeOnce sync.Once
	closeOut := func() {
		closeOnce.Do(func() { close(out) })
	}

	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			ms.mu.Lock()
			for _, unsub := range ms.unsubs {
				unsub()
			}
			ms.mu.Unlock()

			// Deregister the multi-sub.
			b.mu.Lock()
			delete(b.multiSubs, meta.ID)
			b.mu.Unlock()
		})
	}

	// Close out when all fan-in goroutines exit.
	go func() {
		ms.wg.Wait()
		closeOut()
	}()

	return out, func() {
		doUnsub()
		ms.wg.Wait()
		closeOut()
	}
}

// findMultiSub returns the multiSub for the given subscriber ID.
// Caller must hold b.mu (read or write).
func (b *SSEBroker) findMultiSub(subscriberID string) *multiSub {
	return b.multiSubs[subscriberID]
}

// findMultiSubsByScope returns all multiSubs whose ID matches any subscriber
// with the given scope.
func (b *SSEBroker) findMultiSubsByScope(scope string) []*multiSub {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Collect subscriber IDs that match the scope.
	ids := make(map[string]struct{})
	for _, info := range b.subscriberMeta {
		if info != nil && info.Scope == scope && info.ID != "" {
			ids[info.ID] = struct{}{}
		}
	}

	// Also check multiSubs whose underlying channels have matching scope.
	// For multiSubs, we check if any of their topics have scoped subscriptions.
	var result []*multiSub
	for id, ms := range b.multiSubs {
		if _, ok := ids[id]; ok {
			result = append(result, ms)
			continue
		}
		// Check if any of the subscriber's metadata has matching scope.
		ms.mu.Lock()
		for _, ch := range ms.topics {
			if info, ok := b.subscriberMeta[ch]; ok && info.Scope == scope {
				result = append(result, ms)
				break
			}
		}
		ms.mu.Unlock()
	}

	return result
}

// readToBidir finds the bidirectional channel in the broker's topic map that
// corresponds to the read-only channel. Returns nil if not found.
func (b *SSEBroker) readToBidir(topic string, readCh <-chan string) chan string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.findBidirChan(topic, readCh)
}

// findBidirChan searches the topic map for a bidirectional channel matching
// the read-only channel. Caller must hold b.mu.
func (b *SSEBroker) findBidirChan(topic string, readCh <-chan string) chan string {
	for ch := range b.topics[topic] {
		readOnly := (<-chan string)(ch) //nolint:gocritic // parens required for channel type conversion
		if readOnly == readCh {
			return ch
		}
	}
	for ch := range b.scopedTopics[topic] {
		readOnly := (<-chan string)(ch) //nolint:gocritic // parens required for channel type conversion
		if readOnly == readCh {
			return ch
		}
	}
	return nil
}

// sendTopicsChangedEvent sends a control event to the subscriber's output
// channel with the current topic list.
func (b *SSEBroker) sendTopicsChangedEvent(ms *multiSub, action, topic string) {
	ms.mu.Lock()
	topics := make([]string, 0, len(ms.topics))
	for t := range ms.topics {
		topics = append(topics, t)
	}
	ms.mu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"action": action,
		"topic":  topic,
		"topics": topics,
	})

	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", topicsChangedEvent, string(payload))

	select {
	case ms.out <- TopicMessage{Topic: topicsChangedEvent, Data: msg}:
	default:
	}
}
