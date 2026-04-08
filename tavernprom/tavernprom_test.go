package tavernprom_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/tavernprom"
	"github.com/prometheus/client_golang/prometheus"
)

func newBroker(opts ...tavern.BrokerOption) *tavern.SSEBroker {
	return tavern.NewSSEBroker(opts...)
}

func TestRegister(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	reg := prometheus.NewRegistry()

	unreg, err := tavernprom.Register(broker, reg, tavernprom.WithNamespace("test"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer unreg()

	broker.Publish("chat", "hello")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	names := make(map[string]bool)
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}

	for _, want := range []string{
		"test_topics_active",
		"test_subscribers_active",
	} {
		if !names[want] {
			t.Errorf("missing metric %q; got %v", want, names)
		}
	}
}

func TestRegisterDuplicate(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	reg := prometheus.NewRegistry()

	unreg1, err := tavernprom.Register(broker, reg)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	defer unreg1()

	_, err = tavernprom.Register(broker, reg)
	if err == nil {
		t.Fatal("expected error on duplicate Register")
	}
}

func TestHandler(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	broker.Publish("topic1", "msg")

	h := tavernprom.Handler(broker)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "tavern_topics_active") {
		t.Error("response missing tavern_topics_active")
	}
}

func TestObservabilityMetrics(t *testing.T) {
	// Without observability — latency metrics should not appear.
	t.Run("without_observability", func(t *testing.T) {
		broker := newBroker(tavern.WithMetrics())
		reg := prometheus.NewRegistry()
		unreg, err := tavernprom.Register(broker, reg)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		defer unreg()

		broker.Publish("x", "d")

		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			if strings.Contains(mf.GetName(), "latency") {
				t.Errorf("latency metric %q should not appear without observability", mf.GetName())
			}
		}
	})

	// With observability — latency metrics should appear after publish.
	t.Run("with_observability", func(t *testing.T) {
		broker := newBroker(
			tavern.WithMetrics(),
			tavern.WithObservability(tavern.ObservabilityConfig{
				PublishLatency:  true,
				TopicThroughput: true,
			}),
		)
		reg := prometheus.NewRegistry()
		unreg, err := tavernprom.Register(broker, reg)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		defer unreg()

		// Subscribe so publish actually delivers and records latency.
		ch, unsub := broker.Subscribe("latency-topic")
		defer unsub()
		go func() { for range ch { } }()

		// Publish multiple messages.
		for range 5 {
			broker.Publish("latency-topic", "d")
			time.Sleep(time.Millisecond)
		}

		mfs, _ := reg.Gather()
		names := make(map[string]bool)
		for _, mf := range mfs {
			names[mf.GetName()] = true
		}
		if !names["tavern_publish_latency_seconds"] {
			t.Error("missing tavern_publish_latency_seconds with observability enabled")
		}
		if !names["tavern_topic_throughput_messages_per_second"] {
			t.Error("missing tavern_topic_throughput_messages_per_second")
		}
	})
}

func TestCardinalityLimit(t *testing.T) {
	broker := newBroker(tavern.WithMetrics())
	reg := prometheus.NewRegistry()

	unreg, err := tavernprom.Register(broker, reg, tavernprom.WithMaxTopicCardinality(2))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer unreg()

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

	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "tavern_messages_published_total" {
			continue
		}
		topics := make(map[string]bool)
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "topic" {
					topics[lp.GetValue()] = true
				}
			}
		}
		// Should have at most 3 labels: 2 real + "other".
		if len(topics) > 3 {
			t.Errorf("expected at most 3 topic labels (2 + other), got %d: %v", len(topics), topics)
		}
		if !topics["other"] {
			t.Error("expected 'other' overflow label")
		}
	}
}
