package tavern

import "sync"

// publishOp represents a buffered publish operation.
type publishOp struct {
	topic     string
	scope     string // empty for unscoped Publish
	msg       string
	scoped    bool // true for PublishTo/PublishOOBTo
}

// PublishBatch buffers publish operations and flushes them as a single write
// per subscriber. Create one via [SSEBroker.Batch]. Multiple goroutines may
// call Publish/PublishTo concurrently, but Flush and Discard must be called
// at most once and not concurrently with publishes.
type PublishBatch struct {
	broker *SSEBroker
	mu     sync.Mutex
	ops    []publishOp
}

// Batch creates a new [PublishBatch] that buffers publish calls against this
// broker. Call [PublishBatch.Flush] to send all buffered messages or
// [PublishBatch.Discard] to throw them away.
func (b *SSEBroker) Batch() *PublishBatch {
	return &PublishBatch{broker: b}
}

// Publish buffers a message for all subscribers of the given topic.
func (pb *PublishBatch) Publish(topic, msg string) {
	pb.mu.Lock()
	pb.ops = append(pb.ops, publishOp{topic: topic, msg: msg})
	pb.mu.Unlock()
}

// PublishTo buffers a scoped message for subscribers matching the scope.
func (pb *PublishBatch) PublishTo(topic, scope, msg string) {
	pb.mu.Lock()
	pb.ops = append(pb.ops, publishOp{topic: topic, scope: scope, msg: msg, scoped: true})
	pb.mu.Unlock()
}

// PublishOOB buffers OOB fragments for all subscribers of the given topic.
func (pb *PublishBatch) PublishOOB(topic string, fragments ...Fragment) {
	pb.Publish(topic, RenderFragments(fragments...))
}

// PublishOOBTo buffers OOB fragments for scoped subscribers matching the scope.
func (pb *PublishBatch) PublishOOBTo(topic, scope string, fragments ...Fragment) {
	pb.PublishTo(topic, scope, RenderFragments(fragments...))
}

// Discard clears all buffered operations without sending anything.
func (pb *PublishBatch) Discard() {
	pb.mu.Lock()
	pb.ops = nil
	pb.mu.Unlock()
}

// Flush sends all buffered messages. For each subscriber, messages are
// concatenated into a single channel send to minimise SSE writes. Flush
// should be called at most once; the batch is empty afterwards.
func (pb *PublishBatch) Flush() {
	pb.mu.Lock()
	ops := pb.ops
	pb.ops = nil
	pb.mu.Unlock()

	if len(ops) == 0 {
		return
	}

	b := pb.broker

	// Build a map of channel → concatenated message.
	// We need to resolve each op to its target channels.
	chanMsgs := make(map[chan string]string)

	// Track topics touched for metrics.
	type metricsAcc struct {
		sent    int
		dropped int
	}
	topicMetrics := make(map[string]*metricsAcc)

	b.mu.RLock()
	for _, op := range ops {
		if op.scoped {
			scopedSubs := b.scopedTopics[op.topic]
			for ch, sub := range scopedSubs {
				if sub.scope == op.scope {
					chanMsgs[ch] += op.msg
				}
			}
		} else {
			for ch := range b.topics[op.topic] {
				chanMsgs[ch] += op.msg
			}
		}
	}
	b.mu.RUnlock()

	// Now send one concatenated message per channel.
	for ch, msg := range chanMsgs {
		// Determine topic for metrics/eviction by looking up subscriber info.
		topic := ""
		if b.metrics != nil || b.evictThreshold > 0 {
			b.mu.RLock()
			if info, ok := b.subscriberMeta[ch]; ok {
				topic = info.Topic
			}
			b.mu.RUnlock()
		}

		func() {
			defer func() { _ = recover() }()
			if b.send(ch, msg) {
				if b.evictThreshold > 0 {
					b.resetDropCount(ch)
				}
				if b.metrics != nil && topic != "" {
					acc := topicMetrics[topic]
					if acc == nil {
						acc = &metricsAcc{}
						topicMetrics[topic] = acc
					}
					acc.sent++
				}
			} else {
				b.drops.Add(1)
				if b.evictThreshold > 0 && topic != "" {
					if b.incrementDropCount(ch) >= int64(b.evictThreshold) {
						b.evictSubscriber(topic, ch)
					}
				}
				if b.metrics != nil && topic != "" {
					acc := topicMetrics[topic]
					if acc == nil {
						acc = &metricsAcc{}
						topicMetrics[topic] = acc
					}
					acc.dropped++
				}
			}
		}()
	}

	// Update metrics.
	if b.metrics != nil {
		for topic, acc := range topicMetrics {
			tc := b.metrics.counter(topic)
			tc.published.Add(int64(acc.sent))
			tc.dropped.Add(int64(acc.dropped))
		}
	}
}
