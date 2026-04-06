package tavern

import "sync"

// SetOrdered marks or unmarks a topic as ordered. When a topic is ordered,
// concurrent publishes are serialized through a per-topic mutex so that all
// subscribers observe messages in the same order. Non-ordered topics (the
// default) have zero additional synchronization overhead. This method is
// safe for concurrent use.
//
// Call with enabled=true before publishing to guarantee ordering. Call with
// enabled=false to remove the constraint. Toggling while publishes are
// in-flight is safe but ordering is only guaranteed while the topic is
// marked as ordered.
func (b *SSEBroker) SetOrdered(topic string, enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if enabled {
		if b.orderedTopics == nil {
			b.orderedTopics = make(map[string]*sync.Mutex)
		}
		if _, ok := b.orderedTopics[topic]; !ok {
			b.orderedTopics[topic] = &sync.Mutex{}
		}
	} else {
		delete(b.orderedTopics, topic)
	}
}

// orderLock acquires the per-topic ordering mutex if the topic is ordered.
// Returns a function that releases the lock. If the topic is not ordered,
// the returned function is a no-op.
func (b *SSEBroker) orderLock(topic string) func() {
	b.mu.RLock()
	mu, ok := b.orderedTopics[topic]
	b.mu.RUnlock()
	if !ok {
		return func() {}
	}
	mu.Lock()
	return mu.Unlock
}
