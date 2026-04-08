// Package tavernotel exports Tavern broker metrics as OpenTelemetry instruments.
//
// Metrics are collected via asynchronous (observable) instruments whose
// callbacks read from the broker on each OTel collection cycle, so there
// is no background goroutine and no extra overhead between collections.
//
// Usage:
//
//	broker := tavern.NewSSEBroker(tavern.WithMetrics())
//	stop, err := tavernotel.Register(broker, otel.GetMeterProvider())
//	if err != nil { ... }
//	defer stop()
package tavernotel

import (
	"context"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/catgoose/tavern"
)

const (
	defaultMeterName            = "tavern"
	defaultMaxTopicCardinality  = 100
	overflowLabel               = "other"
)

// Option configures the OpenTelemetry exporter.
type Option func(*config)

type config struct {
	meterName          string
	maxTopicCardinality int
}

func defaults() config {
	return config{
		meterName:          defaultMeterName,
		maxTopicCardinality: defaultMaxTopicCardinality,
	}
}

// WithMeterName overrides the default instrumentation scope name (default: "tavern").
func WithMeterName(name string) Option {
	return func(c *config) { c.meterName = name }
}

// WithMaxTopicCardinality limits distinct topic attribute values. Default: 100.
func WithMaxTopicCardinality(n int) Option {
	return func(c *config) { c.maxTopicCardinality = n }
}

// Register registers Tavern metrics as OTel observable instruments on the
// given [metric.MeterProvider]. Callbacks are invoked by the SDK during each
// collection cycle. Returns a stop function that unregisters all callbacks.
func Register(broker *tavern.SSEBroker, mp metric.MeterProvider, opts ...Option) (func(), error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}

	m := mp.Meter(cfg.meterName)
	e := &exporter{broker: broker, cfg: cfg}

	var regs []metric.Registration

	// --- Metrics API instruments ---

	publishedTotal, err := m.Int64ObservableCounter(
		"tavern.messages.published",
		metric.WithDescription("Total messages published per topic."),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		return nil, err
	}

	droppedTotal, err := m.Int64ObservableCounter(
		"tavern.messages.dropped",
		metric.WithDescription("Total messages dropped (subscriber buffer full) per topic."),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		return nil, err
	}

	subscribersPeak, err := m.Int64ObservableGauge(
		"tavern.subscribers.peak",
		metric.WithDescription("Peak concurrent subscribers observed per topic."),
	)
	if err != nil {
		return nil, err
	}

	topicsActive, err := m.Int64ObservableGauge(
		"tavern.topics.active",
		metric.WithDescription("Number of topics with at least one subscriber."),
	)
	if err != nil {
		return nil, err
	}

	subscribersActive, err := m.Int64ObservableGauge(
		"tavern.subscribers.active",
		metric.WithDescription("Total active subscribers across all topics."),
	)
	if err != nil {
		return nil, err
	}

	reg, err := m.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			metrics := e.broker.Metrics()
			stats := e.broker.Stats()

			o.ObserveInt64(topicsActive, int64(stats.Topics))
			o.ObserveInt64(subscribersActive, int64(stats.Subscribers))

			topicCounts := e.broker.TopicCounts()
			allowed := e.limitTopics(metrics.TopicStats, topicCounts)

			for topic, tm := range metrics.TopicStats {
				label := e.resolveLabel(topic, allowed)
				attrs := metric.WithAttributes(attribute.String("topic", label))
				o.ObserveInt64(publishedTotal, tm.Published, attrs)
				o.ObserveInt64(droppedTotal, tm.Dropped, attrs)
				o.ObserveInt64(subscribersPeak, int64(tm.PeakSubscribers), attrs)
			}
			return nil
		},
		publishedTotal, droppedTotal, subscribersPeak, topicsActive, subscribersActive,
	)
	if err != nil {
		return nil, err
	}
	regs = append(regs, reg)

	// --- Observability API instruments (always registered; callbacks no-op when obs disabled) ---

	publishLatency, err := m.Float64ObservableGauge(
		"tavern.publish.latency",
		metric.WithDescription("Publish latency quantiles per topic."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	throughput, err := m.Float64ObservableGauge(
		"tavern.topic.throughput",
		metric.WithDescription("Message throughput per topic (messages/sec)."),
		metric.WithUnit("{message}/s"),
	)
	if err != nil {
		return nil, err
	}

	evictions, err := m.Int64ObservableCounter(
		"tavern.subscriber.evictions",
		metric.WithDescription("Total subscriber evictions per topic."),
	)
	if err != nil {
		return nil, err
	}

	reg2, err := m.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			obs := e.broker.Observability()
			if obs == nil {
				return nil
			}
			snap := obs.Snapshot(e.broker)
			topicCounts := e.broker.TopicCounts()
			allowed := e.limitObsTopics(snap.Topics, topicCounts)

			for topic, to := range snap.Topics {
				label := e.resolveLabel(topic, allowed)
				topicAttr := attribute.String("topic", label)

				if to.PublishLatency.Count > 0 {
					o.ObserveFloat64(publishLatency, to.PublishLatency.P50.Seconds(),
						metric.WithAttributes(topicAttr, attribute.String("quantile", "0.5")))
					o.ObserveFloat64(publishLatency, to.PublishLatency.P95.Seconds(),
						metric.WithAttributes(topicAttr, attribute.String("quantile", "0.95")))
					o.ObserveFloat64(publishLatency, to.PublishLatency.P99.Seconds(),
						metric.WithAttributes(topicAttr, attribute.String("quantile", "0.99")))
				}

				o.ObserveFloat64(throughput, to.Throughput, metric.WithAttributes(topicAttr))
				o.ObserveInt64(evictions, to.EvictionCount, metric.WithAttributes(topicAttr))
			}
			return nil
		},
		publishLatency, throughput, evictions,
	)
	if err != nil {
		return nil, err
	}
	regs = append(regs, reg2)

	stop := func() {
		for _, r := range regs {
			_ = r.Unregister()
		}
	}
	return stop, nil
}

// exporter holds state shared across callbacks.
type exporter struct {
	broker *tavern.SSEBroker
	cfg    config
}

func (e *exporter) limitTopics(topicStats map[string]tavern.TopicMetrics, topicCounts map[string]int) map[string]struct{} {
	if len(topicStats) <= e.cfg.maxTopicCardinality {
		allowed := make(map[string]struct{}, len(topicStats))
		for t := range topicStats {
			allowed[t] = struct{}{}
		}
		return allowed
	}
	return e.rankTopics(topicStats, topicCounts)
}

func (e *exporter) limitObsTopics(topics map[string]tavern.TopicObservability, topicCounts map[string]int) map[string]struct{} {
	if len(topics) <= e.cfg.maxTopicCardinality {
		allowed := make(map[string]struct{}, len(topics))
		for t := range topics {
			allowed[t] = struct{}{}
		}
		return allowed
	}
	// Build a TopicMetrics-shaped map just for ranking.
	fake := make(map[string]tavern.TopicMetrics, len(topics))
	for t := range topics {
		fake[t] = tavern.TopicMetrics{}
	}
	return e.rankTopics(fake, topicCounts)
}

func (e *exporter) rankTopics(topics map[string]tavern.TopicMetrics, topicCounts map[string]int) map[string]struct{} {
	type topicRank struct {
		name  string
		count int
	}
	ranked := make([]topicRank, 0, len(topics))
	for t := range topics {
		ranked = append(ranked, topicRank{name: t, count: topicCounts[t]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].count > ranked[j].count
	})
	allowed := make(map[string]struct{}, e.cfg.maxTopicCardinality)
	for i := 0; i < e.cfg.maxTopicCardinality && i < len(ranked); i++ {
		allowed[ranked[i].name] = struct{}{}
	}
	return allowed
}

func (e *exporter) resolveLabel(topic string, allowed map[string]struct{}) string {
	if _, ok := allowed[topic]; ok {
		return topic
	}
	return overflowLabel
}
