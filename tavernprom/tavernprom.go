// Package tavernprom exports Tavern broker metrics as Prometheus metrics.
//
// Metrics are collected on-demand during each Prometheus scrape via the
// [prometheus.Collector] interface, so there is no background goroutine and
// no extra overhead between scrapes.
//
// Usage:
//
//	broker := tavern.NewSSEBroker(tavern.WithMetrics())
//	unreg, err := tavernprom.Register(broker, prometheus.DefaultRegisterer)
//	if err != nil { ... }
//	defer unreg()
package tavernprom

import (
	"net/http"
	"sort"
	"time"

	"github.com/catgoose/tavern"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultNamespace          = "tavern"
	defaultMaxTopicCardinality = 100
	overflowLabel             = "other"
)

// Option configures the Prometheus exporter.
type Option func(*config)

type config struct {
	namespace          string
	maxTopicCardinality int
}

func defaults() config {
	return config{
		namespace:          defaultNamespace,
		maxTopicCardinality: defaultMaxTopicCardinality,
	}
}

// WithNamespace sets the metric namespace prefix (default: "tavern").
func WithNamespace(ns string) Option {
	return func(c *config) { c.namespace = ns }
}

// WithMaxTopicCardinality limits the number of distinct topic label values
// to prevent cardinality explosion. Topics beyond the limit are grouped
// under "other". Default: 100.
func WithMaxTopicCardinality(n int) Option {
	return func(c *config) { c.maxTopicCardinality = n }
}

// Register registers Tavern metrics with the given Prometheus registerer.
// It returns an unregister function for cleanup.
func Register(broker *tavern.SSEBroker, reg prometheus.Registerer, opts ...Option) (func(), error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}
	c := newCollector(broker, cfg)
	if err := reg.Register(c); err != nil {
		return nil, err
	}
	return func() { reg.Unregister(c) }, nil
}

// Handler returns an [http.Handler] that serves /metrics for the given broker.
// It creates a new [prometheus.Registry], registers the Tavern collector, and
// returns a handler that serves the registry.
func Handler(broker *tavern.SSEBroker, opts ...Option) http.Handler {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}
	reg := prometheus.NewRegistry()
	c := newCollector(broker, cfg)
	reg.MustRegister(c)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// collector implements [prometheus.Collector] for a Tavern broker.
type collector struct {
	broker *tavern.SSEBroker
	cfg    config

	// Metric descriptors.
	publishedTotal   *prometheus.Desc
	droppedTotal     *prometheus.Desc
	subscribersPeak  *prometheus.Desc
	topicsActive     *prometheus.Desc
	subscribersActive *prometheus.Desc

	// Observability-only descriptors (nil-safe: only emitted when obs enabled).
	publishLatency  *prometheus.Desc
	throughput      *prometheus.Desc
	evictionsTotal  *prometheus.Desc
	connDuration    *prometheus.Desc
}

func newCollector(broker *tavern.SSEBroker, cfg config) *collector {
	ns := cfg.namespace
	return &collector{
		broker: broker,
		cfg:    cfg,

		publishedTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "messages_published_total"),
			"Total messages published per topic.",
			[]string{"topic"}, nil,
		),
		droppedTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "messages_dropped_total"),
			"Total messages dropped (subscriber buffer full) per topic.",
			[]string{"topic"}, nil,
		),
		subscribersPeak: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "subscribers_peak"),
			"Peak concurrent subscribers observed per topic.",
			[]string{"topic"}, nil,
		),
		topicsActive: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "topics_active"),
			"Number of topics with at least one subscriber.",
			nil, nil,
		),
		subscribersActive: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "subscribers_active"),
			"Total active subscribers across all topics.",
			nil, nil,
		),

		publishLatency: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "publish_latency_seconds"),
			"Publish latency quantiles per topic.",
			[]string{"topic", "quantile"}, nil,
		),
		throughput: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "topic_throughput_messages_per_second"),
			"Message throughput per topic (messages/sec).",
			[]string{"topic"}, nil,
		),
		evictionsTotal: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "subscriber_evictions_total"),
			"Total subscriber evictions per topic.",
			[]string{"topic"}, nil,
		),
		connDuration: prometheus.NewDesc(
			prometheus.BuildFQName(ns, "", "connection_duration_seconds"),
			"Histogram of subscriber connection durations per topic.",
			[]string{"topic"}, nil,
		),
	}
}

// Describe implements [prometheus.Collector].
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.publishedTotal
	ch <- c.droppedTotal
	ch <- c.subscribersPeak
	ch <- c.topicsActive
	ch <- c.subscribersActive
	ch <- c.publishLatency
	ch <- c.throughput
	ch <- c.evictionsTotal
	ch <- c.connDuration
}

// Collect implements [prometheus.Collector]. It reads the current broker state
// and emits all metric values.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	metrics := c.broker.Metrics()
	stats := c.broker.Stats()

	// Global gauges.
	ch <- prometheus.MustNewConstMetric(c.topicsActive, prometheus.GaugeValue, float64(stats.Topics))
	ch <- prometheus.MustNewConstMetric(c.subscribersActive, prometheus.GaugeValue, float64(stats.Subscribers))

	// Determine which topics to emit (cardinality limiting).
	topicCounts := c.broker.TopicCounts()
	allowedTopics := c.limitTopics(metrics.TopicStats, topicCounts)

	// Aggregate per-topic metrics, combining overflow topics into "other".
	type metricsBucket struct {
		published      float64
		dropped        float64
		peakSubscribers float64
	}
	aggregated := make(map[string]*metricsBucket)
	for topic, tm := range metrics.TopicStats {
		label := c.resolveLabel(topic, allowedTopics)
		b, ok := aggregated[label]
		if !ok {
			b = &metricsBucket{}
			aggregated[label] = b
		}
		b.published += float64(tm.Published)
		b.dropped += float64(tm.Dropped)
		if float64(tm.PeakSubscribers) > b.peakSubscribers {
			b.peakSubscribers = float64(tm.PeakSubscribers)
		}
	}
	for label, b := range aggregated {
		ch <- prometheus.MustNewConstMetric(c.publishedTotal, prometheus.CounterValue, b.published, label)
		ch <- prometheus.MustNewConstMetric(c.droppedTotal, prometheus.CounterValue, b.dropped, label)
		ch <- prometheus.MustNewConstMetric(c.subscribersPeak, prometheus.GaugeValue, b.peakSubscribers, label)
	}

	// Observability metrics (only when enabled).
	obs := c.broker.Observability()
	if obs == nil {
		return
	}
	snap := obs.Snapshot(c.broker)

	// Aggregate observability metrics the same way.
	type obsBucket struct {
		throughput  float64
		evictions   float64
		durations   []time.Duration
		latencyP50  float64
		latencyP95  float64
		latencyP99  float64
		hasLatency  bool
	}
	obsAgg := make(map[string]*obsBucket)
	for topic, to := range snap.Topics {
		label := c.resolveLabel(topic, allowedTopics)
		b, ok := obsAgg[label]
		if !ok {
			b = &obsBucket{}
			obsAgg[label] = b
		}
		b.throughput += to.Throughput
		b.evictions += float64(to.EvictionCount)
		b.durations = append(b.durations, to.ConnectionDurations...)
		if to.PublishLatency.Count > 0 {
			// For overflow, take the max of each quantile as a conservative upper bound.
			if to.PublishLatency.P50.Seconds() > b.latencyP50 {
				b.latencyP50 = to.PublishLatency.P50.Seconds()
			}
			if to.PublishLatency.P95.Seconds() > b.latencyP95 {
				b.latencyP95 = to.PublishLatency.P95.Seconds()
			}
			if to.PublishLatency.P99.Seconds() > b.latencyP99 {
				b.latencyP99 = to.PublishLatency.P99.Seconds()
			}
			b.hasLatency = true
		}
	}
	for label, b := range obsAgg {
		if b.hasLatency {
			ch <- prometheus.MustNewConstMetric(c.publishLatency, prometheus.GaugeValue, b.latencyP50, label, "0.5")
			ch <- prometheus.MustNewConstMetric(c.publishLatency, prometheus.GaugeValue, b.latencyP95, label, "0.95")
			ch <- prometheus.MustNewConstMetric(c.publishLatency, prometheus.GaugeValue, b.latencyP99, label, "0.99")
		}
		ch <- prometheus.MustNewConstMetric(c.throughput, prometheus.GaugeValue, b.throughput, label)
		ch <- prometheus.MustNewConstMetric(c.evictionsTotal, prometheus.CounterValue, b.evictions, label)
		if len(b.durations) > 0 {
			h := buildDurationHistogram(b.durations)
			ch <- prometheus.MustNewConstHistogram(c.connDuration, uint64(len(b.durations)), h.sum, h.buckets, label)
		}
	}
}

// limitTopics returns the set of topics allowed as individual labels.
// Topics are ranked by current subscriber count; the top N are kept.
func (c *collector) limitTopics(topicStats map[string]tavern.TopicMetrics, topicCounts map[string]int) map[string]struct{} {
	if len(topicStats) <= c.cfg.maxTopicCardinality {
		// All fit — return all.
		allowed := make(map[string]struct{}, len(topicStats))
		for t := range topicStats {
			allowed[t] = struct{}{}
		}
		return allowed
	}

	type topicRank struct {
		name  string
		count int
	}
	ranked := make([]topicRank, 0, len(topicStats))
	for t := range topicStats {
		ranked = append(ranked, topicRank{name: t, count: topicCounts[t]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].count > ranked[j].count
	})

	allowed := make(map[string]struct{}, c.cfg.maxTopicCardinality)
	for i := 0; i < c.cfg.maxTopicCardinality && i < len(ranked); i++ {
		allowed[ranked[i].name] = struct{}{}
	}
	return allowed
}

func (c *collector) resolveLabel(topic string, allowed map[string]struct{}) string {
	if _, ok := allowed[topic]; ok {
		return topic
	}
	return overflowLabel
}

// histData holds pre-computed histogram data for MustNewConstHistogram.
type histData struct {
	sum     float64
	buckets map[float64]uint64
}

// buildDurationHistogram builds histogram bucket counts from a slice of durations.
// Uses standard Prometheus-style second-based buckets.
func buildDurationHistogram(durations []time.Duration) histData {
	boundaries := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	buckets := make(map[float64]uint64, len(boundaries))
	for _, b := range boundaries {
		buckets[b] = 0
	}

	var sum float64
	for _, d := range durations {
		s := d.Seconds()
		sum += s
		for _, b := range boundaries {
			if s <= b {
				buckets[b]++
			}
		}
	}
	return histData{sum: sum, buckets: buckets}
}
