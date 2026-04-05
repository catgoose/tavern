package tavern

import (
	"testing"
	"time"
)

func TestObservability_DisabledByDefault(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	if b.Observability() != nil {
		t.Fatal("expected Observability() to return nil when not configured")
	}
}

func TestObservability_ZeroOverheadWhenDisabled(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// These should not panic even without observability.
	b.Publish("events", "msg1")
	b.PublishWithReplay("events", "msg2")
	b.PublishIfChanged("events", "msg3")
	b.PublishTo("events", "scope", "msg4")

	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case <-ch:
		case <-timeout:
			return
		}
	}
}

func TestObservability_PublishLatency(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency: true,
	}))
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	for i := 0; i < 10; i++ {
		b.Publish("events", "msg")
	}
	for i := 0; i < 10; i++ {
		<-ch
	}

	obs := b.Observability()
	p99 := obs.PublishLatencyP99("events")
	if p99 <= 0 {
		t.Fatal("expected positive p99 latency")
	}
}

func TestObservability_PublishLatency_NoDataReturnsZero(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency: true,
	}))
	defer b.Close()

	obs := b.Observability()
	if p99 := obs.PublishLatencyP99("nonexistent"); p99 != 0 {
		t.Fatalf("expected 0 for nonexistent topic, got %v", p99)
	}
}

func TestObservability_PublishLatency_DisabledReturnsZero(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency: false,
	}))
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()
	b.Publish("events", "msg")
	<-ch

	obs := b.Observability()
	if p99 := obs.PublishLatencyP99("events"); p99 != 0 {
		t.Fatalf("expected 0 when PublishLatency disabled, got %v", p99)
	}
}

func TestObservability_SubscriberLag(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		SubscriberLag: true,
	}), WithBufferSize(10))
	defer b.Close()

	_, unsub := b.SubscribeWithMeta("events", SubscribeMeta{ID: "sub-1"})
	defer unsub()

	// Publish 5 messages without draining.
	for i := 0; i < 5; i++ {
		b.Publish("events", "msg")
	}

	obs := b.Observability()
	lag := obs.SubscriberLag("events", b)
	if lag == nil {
		t.Fatal("expected non-nil lag map")
	}
	depth, ok := lag["sub-1"]
	if !ok {
		t.Fatal("expected sub-1 in lag map")
	}
	if depth != 5 {
		t.Fatalf("expected lag=5, got %d", depth)
	}
}

func TestObservability_SubscriberLag_DisabledReturnsNil(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		SubscriberLag: false,
	}))
	defer b.Close()

	obs := b.Observability()
	if lag := obs.SubscriberLag("events", b); lag != nil {
		t.Fatal("expected nil when SubscriberLag disabled")
	}
}

func TestObservability_ConnectionDuration(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		ConnectionDuration: true,
	}))
	defer b.Close()

	_, unsub := b.Subscribe("events")
	time.Sleep(10 * time.Millisecond)
	unsub()

	obs := b.Observability()
	durations := obs.ConnectionDurations("events")
	if len(durations) != 1 {
		t.Fatalf("expected 1 duration, got %d", len(durations))
	}
	if durations[0] < 10*time.Millisecond {
		t.Fatalf("expected duration >= 10ms, got %v", durations[0])
	}
}

func TestObservability_ConnectionDuration_ScopedSubscriber(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		ConnectionDuration: true,
	}))
	defer b.Close()

	_, unsub := b.SubscribeScoped("events", "scope-1")
	time.Sleep(10 * time.Millisecond)
	unsub()

	obs := b.Observability()
	durations := obs.ConnectionDurations("events")
	if len(durations) != 1 {
		t.Fatalf("expected 1 duration, got %d", len(durations))
	}
	if durations[0] < 10*time.Millisecond {
		t.Fatalf("expected duration >= 10ms, got %v", durations[0])
	}
}

func TestObservability_ConnectionDuration_DisabledReturnsNil(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		ConnectionDuration: false,
	}))
	defer b.Close()

	_, unsub := b.Subscribe("events")
	unsub()

	obs := b.Observability()
	if d := obs.ConnectionDurations("events"); d != nil {
		t.Fatal("expected nil when ConnectionDuration disabled")
	}
}

func TestObservability_TopicThroughput(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		TopicThroughput: true,
	}), WithBufferSize(100))
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	for i := 0; i < 50; i++ {
		b.Publish("events", "msg")
	}
	for i := 0; i < 50; i++ {
		<-ch
	}

	obs := b.Observability()
	rate := obs.TopicThroughput("events")
	if rate <= 0 {
		t.Fatal("expected positive throughput")
	}
}

func TestObservability_TopicThroughput_DisabledReturnsZero(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		TopicThroughput: false,
	}))
	defer b.Close()

	obs := b.Observability()
	if rate := obs.TopicThroughput("events"); rate != 0 {
		t.Fatalf("expected 0 when TopicThroughput disabled, got %v", rate)
	}
}

func TestObservability_EvictionCount(t *testing.T) {
	b := NewSSEBroker(
		WithObservability(ObservabilityConfig{}),
		WithBufferSize(1),
		WithSlowSubscriberEviction(2),
	)
	defer b.Close()

	_, unsub := b.Subscribe("events")
	defer unsub()

	// Fill buffer and cause drops that trigger eviction.
	for i := 0; i < 10; i++ {
		b.Publish("events", "msg")
	}

	// Allow eviction to complete.
	time.Sleep(50 * time.Millisecond)

	obs := b.Observability()
	count := obs.EvictionCount("events")
	if count < 1 {
		t.Fatalf("expected at least 1 eviction, got %d", count)
	}
}

func TestObservability_Snapshot(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency:     true,
		SubscriberLag:      true,
		ConnectionDuration: true,
		TopicThroughput:    true,
	}))
	defer b.Close()

	// Create subscriber, publish, then disconnect to populate all metrics.
	ch, unsub := b.SubscribeWithMeta("events", SubscribeMeta{ID: "snap-sub"})
	for i := 0; i < 5; i++ {
		b.Publish("events", "msg")
	}
	for i := 0; i < 5; i++ {
		<-ch
	}
	unsub()

	obs := b.Observability()
	snap := obs.Snapshot(b)

	if snap.Topics == nil {
		t.Fatal("expected non-nil Topics in snapshot")
	}
	to, ok := snap.Topics["events"]
	if !ok {
		t.Fatal("expected 'events' topic in snapshot")
	}
	if to.PublishLatency.Count != 5 {
		t.Fatalf("expected latency count=5, got %d", to.PublishLatency.Count)
	}
	if to.PublishLatency.P50 <= 0 || to.PublishLatency.P95 <= 0 || to.PublishLatency.P99 <= 0 {
		t.Fatal("expected positive latency percentiles")
	}
	if to.Throughput <= 0 {
		t.Fatal("expected positive throughput in snapshot")
	}
	if len(to.ConnectionDurations) != 1 {
		t.Fatalf("expected 1 connection duration, got %d", len(to.ConnectionDurations))
	}
}

func TestObservability_Snapshot_EmptyWhenDisabled(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{}))
	defer b.Close()

	obs := b.Observability()
	snap := obs.Snapshot(b)
	if len(snap.Topics) != 0 {
		t.Fatalf("expected empty snapshot, got %d topics", len(snap.Topics))
	}
}

func TestObservability_PublishWithReplay_RecordsLatency(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency:  true,
		TopicThroughput: true,
	}))
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	b.PublishWithReplay("events", "msg")
	<-ch

	obs := b.Observability()
	if p99 := obs.PublishLatencyP99("events"); p99 <= 0 {
		t.Fatal("expected positive p99 from PublishWithReplay")
	}
	if rate := obs.TopicThroughput("events"); rate <= 0 {
		t.Fatal("expected positive throughput from PublishWithReplay")
	}
}

func TestObservability_PublishTo_RecordsLatency(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		PublishLatency:  true,
		TopicThroughput: true,
	}))
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "scope-1")
	defer unsub()

	b.PublishTo("events", "scope-1", "msg")
	<-ch

	obs := b.Observability()
	if p99 := obs.PublishLatencyP99("events"); p99 <= 0 {
		t.Fatal("expected positive p99 from PublishTo")
	}
}

func TestObservability_SubscriberLag_FallbackID(t *testing.T) {
	b := NewSSEBroker(WithObservability(ObservabilityConfig{
		SubscriberLag: true,
	}), WithBufferSize(10))
	defer b.Close()

	// Subscribe without meta — should use ConnectedAt as fallback ID.
	_, unsub := b.Subscribe("events")
	defer unsub()

	b.Publish("events", "msg")

	obs := b.Observability()
	lag := obs.SubscriberLag("events", b)
	if len(lag) != 1 {
		t.Fatalf("expected 1 entry in lag map, got %d", len(lag))
	}
	for _, depth := range lag {
		if depth != 1 {
			t.Fatalf("expected lag=1, got %d", depth)
		}
	}
}

func TestPercentile_EmptySlice(t *testing.T) {
	if d := percentile(nil, 0.99); d != 0 {
		t.Fatalf("expected 0 for empty slice, got %v", d)
	}
}

func TestPercentile_SingleElement(t *testing.T) {
	samples := []time.Duration{5 * time.Millisecond}
	if d := percentile(samples, 0.50); d != 5*time.Millisecond {
		t.Fatalf("expected 5ms, got %v", d)
	}
	if d := percentile(samples, 0.99); d != 5*time.Millisecond {
		t.Fatalf("expected 5ms, got %v", d)
	}
}

func TestPercentile_MultipleElements(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	p50 := percentile(samples, 0.50)
	p99 := percentile(samples, 0.99)
	if p50 != 50*time.Millisecond {
		t.Fatalf("expected p50=50ms, got %v", p50)
	}
	if p99 != 99*time.Millisecond {
		t.Fatalf("expected p99=99ms, got %v", p99)
	}
}

func TestCircularDurations_Wraps(t *testing.T) {
	c := newCircularDurations(4)
	for i := 0; i < 10; i++ {
		c.add(time.Duration(i) * time.Millisecond)
	}
	snap, count := c.snapshot()
	if count != 10 {
		t.Fatalf("expected count=10, got %d", count)
	}
	if len(snap) != 4 {
		t.Fatalf("expected 4 samples after wrap, got %d", len(snap))
	}
}

func TestSlidingWindow_ExpiresOldEvents(t *testing.T) {
	sw := newSlidingWindow(100 * time.Millisecond)
	now := time.Now()
	// Add events in the past.
	sw.record(now.Add(-200 * time.Millisecond))
	sw.record(now.Add(-150 * time.Millisecond))
	// Add current events.
	sw.record(now)
	sw.record(now)

	rate := sw.rate(now)
	// Only 2 events should be in the window.
	expected := 2.0 / 0.1 // 20 msgs/sec
	if rate != expected {
		t.Fatalf("expected rate=%.1f, got %.1f", expected, rate)
	}
}
