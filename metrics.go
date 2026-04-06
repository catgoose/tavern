package tavern

import (
	"sync"
	"sync/atomic"
)

// TopicMetrics holds per-topic counters. All fields are cumulative since the
// broker was created (except PeakSubscribers which is a high-water mark).
type TopicMetrics struct {
	// Published is the total number of messages successfully delivered to at
	// least one subscriber on this topic.
	Published int64
	// Dropped is the total number of delivery failures (subscriber buffer full)
	// on this topic.
	Dropped int64
	// PeakSubscribers is the highest number of concurrent subscribers observed
	// on this topic since the broker was created.
	PeakSubscribers int
}

// BrokerMetrics is a point-in-time snapshot of all broker metrics.
type BrokerMetrics struct {
	// TopicStats maps topic name to its metrics.
	TopicStats map[string]TopicMetrics
	// TotalPublished is the sum of all per-topic published counts.
	TotalPublished int64
	// TotalDropped is the sum of all per-topic dropped counts.
	TotalDropped int64
}

// topicCounter tracks per-topic atomic counters.
type topicCounter struct {
	published atomic.Int64
	dropped   atomic.Int64
}

// metricsState holds all metrics tracking state.
type metricsState struct {
	mu       sync.RWMutex
	counters map[string]*topicCounter
	peakSubs map[string]int // protected by broker's mu, updated on subscribe
}

// counter returns the topicCounter for the given topic, creating one if needed.
func (m *metricsState) counter(topic string) *topicCounter {
	m.mu.RLock()
	tc, ok := m.counters[topic]
	m.mu.RUnlock()
	if ok {
		return tc
	}
	m.mu.Lock()
	tc, ok = m.counters[topic]
	if !ok {
		tc = &topicCounter{}
		m.counters[topic] = tc
	}
	m.mu.Unlock()
	return tc
}

// WithMetrics enables per-topic publish and drop counters. When disabled
// (the default), metrics tracking has zero overhead. Use [SSEBroker.Metrics]
// to retrieve a snapshot.
func WithMetrics() BrokerOption {
	return func(b *SSEBroker) {
		b.metrics = &metricsState{
			counters: make(map[string]*topicCounter),
			peakSubs: make(map[string]int),
		}
	}
}

// Metrics returns a point-in-time snapshot of per-topic and aggregate
// metrics. Returns an empty [BrokerMetrics] if metrics are not enabled
// (see [WithMetrics]).
func (b *SSEBroker) Metrics() BrokerMetrics {
	if b.metrics == nil {
		return BrokerMetrics{TopicStats: make(map[string]TopicMetrics)}
	}

	// Snapshot peak subs under broker lock to avoid lock ordering issues.
	b.mu.RLock()
	peakSnap := make(map[string]int, len(b.metrics.peakSubs))
	for t, p := range b.metrics.peakSubs {
		peakSnap[t] = p
	}
	b.mu.RUnlock()

	// Snapshot counters under metrics lock.
	b.metrics.mu.RLock()
	result := BrokerMetrics{
		TopicStats: make(map[string]TopicMetrics, len(b.metrics.counters)),
	}
	for topic, tc := range b.metrics.counters {
		published := tc.published.Load()
		dropped := tc.dropped.Load()
		result.TopicStats[topic] = TopicMetrics{
			Published:       published,
			Dropped:         dropped,
			PeakSubscribers: peakSnap[topic],
		}
		result.TotalPublished += published
		result.TotalDropped += dropped
	}
	b.metrics.mu.RUnlock()
	return result
}
