package tavern

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultHistogramSize is the number of samples kept in a circular buffer.
	defaultHistogramSize = 1024

	// defaultThroughputWindow is the sliding window for throughput calculation.
	defaultThroughputWindow = 10 * time.Second
)

// ObservabilityConfig controls which observability features are enabled.
// By default all fields are false and observability has zero overhead.
// Enable individual features selectively to minimize runtime cost.
type ObservabilityConfig struct {
	// PublishLatency enables per-topic publish latency histograms.
	PublishLatency bool
	// SubscriberLag enables per-subscriber buffer depth gauges.
	SubscriberLag bool
	// ConnectionDuration enables tracking of subscriber connection durations.
	ConnectionDuration bool
	// TopicThroughput enables per-topic message rate calculation.
	TopicThroughput bool
}

// LatencyHistogram holds percentile latency data computed from a circular
// buffer of the most recent 1024 samples.
type LatencyHistogram struct {
	P50, P95, P99 time.Duration
	Count         int64
}

// TopicObservability holds observability data for a single topic. All fields
// are populated based on the features enabled in [ObservabilityConfig]; disabled
// features produce zero values.
type TopicObservability struct {
	PublishLatency      LatencyHistogram
	SubscriberLag       map[string]int // subscriberID -> buffer depth
	ConnectionDurations []time.Duration
	Throughput          float64 // msgs/sec over last window
	EvictionCount       int64
}

// ObservabilitySnapshot is a point-in-time, export-friendly snapshot of all
// observability data across all topics. Obtain one via
// [observabilityState.Snapshot].
type ObservabilitySnapshot struct {
	Topics map[string]TopicObservability
}

// observabilityState holds internal state for the observability subsystem.
type observabilityState struct {
	config ObservabilityConfig

	// Per-topic latency circular buffers.
	latencyMu      sync.RWMutex
	latencyBuffers map[string]*circularDurations

	// Per-topic throughput sliding window.
	throughputMu   sync.RWMutex
	throughputData map[string]*slidingWindow

	// Per-topic connection durations (recorded on unsubscribe).
	connDurMu   sync.RWMutex
	connDurData map[string]*circularDurations

	// Per-topic eviction counters.
	evictionMu       sync.RWMutex
	evictionCounters map[string]*atomic.Int64
}

// circularDurations is a fixed-size circular buffer for time.Duration values.
type circularDurations struct {
	mu    sync.Mutex
	buf   []time.Duration
	pos   int
	count int64
	full  bool
}

func newCircularDurations(size int) *circularDurations {
	return &circularDurations{
		buf: make([]time.Duration, size),
	}
}

func (c *circularDurations) add(d time.Duration) {
	c.mu.Lock()
	c.buf[c.pos] = d
	c.pos = (c.pos + 1) % len(c.buf)
	c.count++
	if !c.full && c.count >= int64(len(c.buf)) {
		c.full = true
	}
	c.mu.Unlock()
}

// snapshot returns a copy of the stored durations (unordered).
func (c *circularDurations) snapshot() (samples []time.Duration, total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := int(c.count)
	if c.full {
		n = len(c.buf)
	}
	out := make([]time.Duration, n)
	copy(out, c.buf[:n])
	return out, c.count
}

// slidingWindow tracks timestamps of events for rate calculation.
type slidingWindow struct {
	mu     sync.Mutex
	events []time.Time
	window time.Duration
}

func newSlidingWindow(window time.Duration) *slidingWindow {
	return &slidingWindow{
		window: window,
	}
}

func (sw *slidingWindow) record(now time.Time) {
	sw.mu.Lock()
	sw.events = append(sw.events, now)
	sw.gc(now)
	sw.mu.Unlock()
}

func (sw *slidingWindow) rate(now time.Time) float64 {
	sw.mu.Lock()
	sw.gc(now)
	n := len(sw.events)
	sw.mu.Unlock()
	if n == 0 {
		return 0
	}
	return float64(n) / sw.window.Seconds()
}

// gc removes events outside the window. Must be called with mu held.
func (sw *slidingWindow) gc(now time.Time) {
	cutoff := now.Add(-sw.window)
	i := 0
	for i < len(sw.events) && sw.events[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		sw.events = sw.events[i:]
	}
}

// WithObservability enables enhanced observability with the given configuration.
// Disabled by default — zero overhead when not configured.
func WithObservability(config ObservabilityConfig) BrokerOption {
	return func(b *SSEBroker) {
		b.obs = &observabilityState{
			config:           config,
			latencyBuffers:   make(map[string]*circularDurations),
			throughputData:   make(map[string]*slidingWindow),
			connDurData:      make(map[string]*circularDurations),
			evictionCounters: make(map[string]*atomic.Int64),
		}
	}
}

// Observability returns the observability handle for the broker.
// Returns nil if observability is not enabled.
func (b *SSEBroker) Observability() *observabilityState {
	return b.obs
}

// recordPublishLatency records a publish latency sample for a topic.
func (o *observabilityState) recordPublishLatency(topic string, d time.Duration) {
	if !o.config.PublishLatency {
		return
	}
	buf := o.getOrCreateLatencyBuffer(topic)
	buf.add(d)
}

func (o *observabilityState) getOrCreateLatencyBuffer(topic string) *circularDurations {
	o.latencyMu.RLock()
	buf, ok := o.latencyBuffers[topic]
	o.latencyMu.RUnlock()
	if ok {
		return buf
	}
	o.latencyMu.Lock()
	buf, ok = o.latencyBuffers[topic]
	if !ok {
		buf = newCircularDurations(defaultHistogramSize)
		o.latencyBuffers[topic] = buf
	}
	o.latencyMu.Unlock()
	return buf
}

// recordThroughput records a publish event for throughput calculation.
func (o *observabilityState) recordThroughput(topic string, now time.Time) {
	if !o.config.TopicThroughput {
		return
	}
	sw := o.getOrCreateSlidingWindow(topic)
	sw.record(now)
}

func (o *observabilityState) getOrCreateSlidingWindow(topic string) *slidingWindow {
	o.throughputMu.RLock()
	sw, ok := o.throughputData[topic]
	o.throughputMu.RUnlock()
	if ok {
		return sw
	}
	o.throughputMu.Lock()
	sw, ok = o.throughputData[topic]
	if !ok {
		sw = newSlidingWindow(defaultThroughputWindow)
		o.throughputData[topic] = sw
	}
	o.throughputMu.Unlock()
	return sw
}

// recordConnectionDuration records a connection duration when a subscriber disconnects.
func (o *observabilityState) recordConnectionDuration(topic string, d time.Duration) {
	if !o.config.ConnectionDuration {
		return
	}
	buf := o.getOrCreateConnDurBuffer(topic)
	buf.add(d)
}

func (o *observabilityState) getOrCreateConnDurBuffer(topic string) *circularDurations {
	o.connDurMu.RLock()
	buf, ok := o.connDurData[topic]
	o.connDurMu.RUnlock()
	if ok {
		return buf
	}
	o.connDurMu.Lock()
	buf, ok = o.connDurData[topic]
	if !ok {
		buf = newCircularDurations(defaultHistogramSize)
		o.connDurData[topic] = buf
	}
	o.connDurMu.Unlock()
	return buf
}

// recordEviction increments the eviction counter for a topic.
func (o *observabilityState) recordEviction(topic string) {
	o.evictionMu.RLock()
	counter, ok := o.evictionCounters[topic]
	o.evictionMu.RUnlock()
	if ok {
		counter.Add(1)
		return
	}
	o.evictionMu.Lock()
	counter, ok = o.evictionCounters[topic]
	if !ok {
		counter = &atomic.Int64{}
		o.evictionCounters[topic] = counter
	}
	o.evictionMu.Unlock()
	counter.Add(1)
}

// PublishLatencyP99 returns the p99 publish latency for the given topic.
func (o *observabilityState) PublishLatencyP99(topic string) time.Duration {
	if !o.config.PublishLatency {
		return 0
	}
	o.latencyMu.RLock()
	buf, ok := o.latencyBuffers[topic]
	o.latencyMu.RUnlock()
	if !ok {
		return 0
	}
	samples, _ := buf.snapshot()
	return percentile(samples, 0.99)
}

// SubscriberLag returns the buffer depth for each subscriber on the given topic.
// The broker reference is needed to inspect subscriber channels.
func (o *observabilityState) SubscriberLag(topic string, b *SSEBroker) map[string]int {
	if !o.config.SubscriberLag {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]int)
	for ch := range b.topics[topic] {
		info, ok := b.subscriberMeta[ch]
		if !ok {
			continue
		}
		id := info.ID
		if id == "" {
			id = info.ConnectedAt.Format(time.RFC3339Nano)
		}
		result[id] = len(ch)
	}
	for ch := range b.scopedTopics[topic] {
		info, ok := b.subscriberMeta[ch]
		if !ok {
			continue
		}
		id := info.ID
		if id == "" {
			id = info.ConnectedAt.Format(time.RFC3339Nano)
		}
		result[id] = len(ch)
	}
	return result
}

// ConnectionDurations returns recorded connection durations for the given topic.
func (o *observabilityState) ConnectionDurations(topic string) []time.Duration {
	if !o.config.ConnectionDuration {
		return nil
	}
	o.connDurMu.RLock()
	buf, ok := o.connDurData[topic]
	o.connDurMu.RUnlock()
	if !ok {
		return nil
	}
	durations, _ := buf.snapshot()
	return durations
}

// TopicThroughput returns the message rate (msgs/sec) for the given topic.
func (o *observabilityState) TopicThroughput(topic string) float64 {
	if !o.config.TopicThroughput {
		return 0
	}
	o.throughputMu.RLock()
	sw, ok := o.throughputData[topic]
	o.throughputMu.RUnlock()
	if !ok {
		return 0
	}
	return sw.rate(time.Now())
}

// EvictionCount returns the number of subscriber evictions for the given topic.
func (o *observabilityState) EvictionCount(topic string) int64 {
	o.evictionMu.RLock()
	counter, ok := o.evictionCounters[topic]
	o.evictionMu.RUnlock()
	if !ok {
		return 0
	}
	return counter.Load()
}

// Snapshot returns a point-in-time snapshot of all observability data.
func (o *observabilityState) Snapshot(b *SSEBroker) ObservabilitySnapshot {
	snap := ObservabilitySnapshot{
		Topics: make(map[string]TopicObservability),
	}
	now := time.Now()

	// Collect all known topics across all data sources.
	seen := make(map[string]struct{})
	if o.config.PublishLatency {
		o.latencyMu.RLock()
		for t := range o.latencyBuffers {
			seen[t] = struct{}{}
		}
		o.latencyMu.RUnlock()
	}
	if o.config.TopicThroughput {
		o.throughputMu.RLock()
		for t := range o.throughputData {
			seen[t] = struct{}{}
		}
		o.throughputMu.RUnlock()
	}
	if o.config.ConnectionDuration {
		o.connDurMu.RLock()
		for t := range o.connDurData {
			seen[t] = struct{}{}
		}
		o.connDurMu.RUnlock()
	}
	o.evictionMu.RLock()
	for t := range o.evictionCounters {
		seen[t] = struct{}{}
	}
	o.evictionMu.RUnlock()

	// Also include topics with active subscribers.
	if o.config.SubscriberLag {
		b.mu.RLock()
		for t := range b.topics {
			seen[t] = struct{}{}
		}
		for t := range b.scopedTopics {
			seen[t] = struct{}{}
		}
		b.mu.RUnlock()
	}

	for topic := range seen {
		to := TopicObservability{}

		if o.config.PublishLatency {
			o.latencyMu.RLock()
			buf, ok := o.latencyBuffers[topic]
			o.latencyMu.RUnlock()
			if ok {
				samples, count := buf.snapshot()
				to.PublishLatency = LatencyHistogram{
					P50:   percentile(samples, 0.50),
					P95:   percentile(samples, 0.95),
					P99:   percentile(samples, 0.99),
					Count: count,
				}
			}
		}

		if o.config.SubscriberLag {
			to.SubscriberLag = o.SubscriberLag(topic, b)
		}

		if o.config.ConnectionDuration {
			to.ConnectionDurations = o.ConnectionDurations(topic)
		}

		if o.config.TopicThroughput {
			o.throughputMu.RLock()
			sw, ok := o.throughputData[topic]
			o.throughputMu.RUnlock()
			if ok {
				to.Throughput = sw.rate(now)
			}
		}

		o.evictionMu.RLock()
		counter, ok := o.evictionCounters[topic]
		o.evictionMu.RUnlock()
		if ok {
			to.EvictionCount = counter.Load()
		}

		snap.Topics[topic] = to
	}
	return snap
}

// percentile computes the given percentile (0..1) from a slice of durations.
// Returns 0 if the slice is empty.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	slices.Sort(samples)
	idx := int(math.Ceil(p*float64(len(samples)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}
