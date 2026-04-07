package tavern

import "sync"

// publishOp represents a buffered publish operation.
type publishOp struct {
	topic     string
	scope     string // empty for unscoped Publish
	msg       string
	scoped    bool // true for PublishTo/PublishOOBTo
}

// PublishBatch buffers publish operations and flushes them as a single
// concatenated write per subscriber channel, reducing the number of SSE
// writes on the wire. Create one via [SSEBroker.Batch]. Multiple goroutines
// may call Publish/PublishTo concurrently, but Flush and Discard must be
// called at most once and not concurrently with publishes.
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

// Flush sends all buffered messages. For each unique (topic, scope)
// combination the individual messages are concatenated, then routed through
// the same publish pipeline as [SSEBroker.Publish] / [SSEBroker.PublishTo]
// — middleware, rate limiting, filters, adaptive backpressure, glob
// subscribers, backend publish, observability, and after-hooks all execute
// exactly as they would for a regular publish. The concatenation preserves
// the batch's value proposition: each subscriber receives a single channel
// write containing all messages for that topic.
//
// Flush should be called at most once; the batch is empty afterwards.
func (pb *PublishBatch) Flush() {
	pb.mu.Lock()
	ops := pb.ops
	pb.ops = nil
	pb.mu.Unlock()

	if len(ops) == 0 {
		return
	}

	// Group messages by (topic, scope, scoped) and concatenate in order.
	type groupKey struct {
		topic  string
		scope  string
		scoped bool
	}
	type group struct {
		key groupKey
		msg string
	}
	seen := make(map[groupKey]int) // key → index in groups slice
	var groups []group

	for _, op := range ops {
		k := groupKey{topic: op.topic, scope: op.scope, scoped: op.scoped}
		if idx, ok := seen[k]; ok {
			groups[idx].msg += op.msg
		} else {
			seen[k] = len(groups)
			groups = append(groups, group{key: k, msg: op.msg})
		}
	}

	b := pb.broker

	// Dispatch each group through the normal publish path. The middleware,
	// publishToChannels, glob dispatch, backend publish, observability, and
	// after-hooks all fire exactly as they do for Publish/PublishTo.
	for _, g := range groups {
		if g.key.scoped {
			b.PublishTo(g.key.topic, g.key.scope, g.msg)
		} else {
			b.Publish(g.key.topic, g.msg)
		}
	}
}
