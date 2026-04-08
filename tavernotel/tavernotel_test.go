package tavernotel_test

import (
	"context"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/tavernotel"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newBroker(opts ...tavern.BrokerOption) *tavern.SSEBroker {
	return tavern.NewSSEBroker(opts...)
}

// collectMetrics creates an in-memory reader, registers the broker, triggers
// a collection, and returns the collected resource metrics.
func collectMetrics(t *testing.T, broker *tavern.SSEBroker, opts ...tavernotel.Option) (metricdata.ResourceMetrics, func()) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	stop, err := tavernotel.Register(broker, mp, opts...)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm, func() {
		stop()
		_ = mp.Shutdown(context.Background())
	}
}

func metricNames(rm metricdata.ResourceMetrics) map[string]bool {
	names := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

func TestRegister(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	ch, unsub := broker.Subscribe("chat")
	defer unsub()
	go func() { for range ch { } }()
	broker.Publish("chat", "hello")

	rm, stop := collectMetrics(t, broker, tavernotel.WithMeterName("test"))
	defer stop()

	names := metricNames(rm)
	for _, want := range []string{
		"tavern.topics.active",
		"tavern.subscribers.active",
		"tavern.messages.published",
	} {
		if !names[want] {
			t.Errorf("missing metric %q; got %v", want, names)
		}
	}
}

func TestObservabilityMetrics(t *testing.T) {
	t.Run("without_observability", func(t *testing.T) {
		broker := newBroker(tavern.WithMetrics())
		broker.Publish("x", "d")

		rm, stop := collectMetrics(t, broker)
		defer stop()

		names := metricNames(rm)
		// Latency instrument is registered but callback should produce no data points
		// when observability is nil. The instrument itself may still appear with no data.
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "tavern.publish.latency" {
					if g, ok := m.Data.(metricdata.Gauge[float64]); ok {
						if len(g.DataPoints) > 0 {
							t.Error("publish latency should have no data points without observability")
						}
					}
				}
			}
		}
		_ = names
	})

	t.Run("with_observability", func(t *testing.T) {
		broker := newBroker(
			tavern.WithMetrics(),
			tavern.WithObservability(tavern.ObservabilityConfig{
				PublishLatency:  true,
				TopicThroughput: true,
			}),
		)

		ch, unsub := broker.Subscribe("latency-topic")
		defer unsub()
		go func() { for range ch { } }()

		for range 5 {
			broker.Publish("latency-topic", "d")
			time.Sleep(time.Millisecond)
		}

		rm, stop := collectMetrics(t, broker)
		defer stop()

		names := metricNames(rm)
		if !names["tavern.publish.latency"] {
			t.Error("missing tavern.publish.latency with observability enabled")
		}
		if !names["tavern.topic.throughput"] {
			t.Error("missing tavern.topic.throughput")
		}
	})
}

func TestCardinalityLimit(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())

	// Subscribe and publish to 5 topics so per-topic metrics exist.
	var unsubs []func()
	for _, topic := range []string{"a", "b", "c", "d", "e"} {
		ch, unsub := broker.Subscribe(topic)
		unsubs = append(unsubs, unsub)
		go func() { for range ch {} }()
		broker.Publish(topic, "x")
	}
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()

	rm, stop := collectMetrics(t, broker, tavernotel.WithMaxTopicCardinality(2))
	defer stop()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "tavern.messages.published" {
				continue
			}
			topics := make(map[string]bool)
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range s.DataPoints {
					for _, kv := range dp.Attributes.ToSlice() {
						if kv.Key == attribute.Key("topic") {
							topics[kv.Value.AsString()] = true
						}
					}
				}
			}
			if len(topics) > 3 {
				t.Errorf("expected at most 3 topic attrs (2 + other), got %d: %v", len(topics), topics)
			}
			if !topics["other"] {
				t.Error("expected 'other' overflow attribute")
			}
		}
	}
}

func TestStop(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	stop, err := tavernotel.Register(broker, mp)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Should not panic after stop.
	stop()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect after stop: %v", err)
	}
}
