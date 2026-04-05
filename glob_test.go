package tavern

import (
	"sync"
	"testing"
	"time"
)

// --- matchGlob unit tests ---

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		topic   string
		want    bool
	}{
		// Exact match
		{"app/dashboard/metrics", "app/dashboard/metrics", true},
		{"app", "app", true},
		{"app/dashboard/metrics", "app/dashboard/alerts", false},

		// Single-level wildcard
		{"app/dashboard/*", "app/dashboard/metrics", true},
		{"app/dashboard/*", "app/dashboard/alerts", true},
		{"app/dashboard/*", "app/dashboard/metrics/cpu", false},
		{"app/*/status", "app/orders/status", true},
		{"app/*/status", "app/users/status", true},
		{"app/*/status", "app/orders/detail/status", false},
		{"*/dashboard", "app/dashboard", true},
		{"*/dashboard", "dashboard", false},

		// Multi-level wildcard
		{"app/dashboard/**", "app/dashboard", true},
		{"app/dashboard/**", "app/dashboard/metrics", true},
		{"app/dashboard/**", "app/dashboard/metrics/cpu", true},
		{"app/dashboard/**", "app/other", false},
		{"**", "anything", true},
		{"**", "a/b/c/d", true},
		{"**", "app", true},

		// Cross-hierarchy wildcard
		{"**/alerts", "alerts", true},
		{"**/alerts", "app/alerts", true},
		{"**/alerts", "app/dashboard/alerts", true},
		{"**/alerts", "app/dashboard/alerts/critical", false},
		{"**/alerts/critical", "alerts/critical", true},
		{"**/alerts/critical", "app/alerts/critical", true},
		{"**/alerts/critical", "app/dashboard/alerts/critical", true},
		{"**/alerts/critical", "alerts", false},

		// Mixed wildcards
		{"app/**/status", "app/status", true},
		{"app/**/status", "app/orders/status", true},
		{"app/**/status", "app/orders/123/status", true},
		{"app/*/events/**", "app/orders/events", true},
		{"app/*/events/**", "app/orders/events/click", true},
		{"app/*/events/**", "app/orders/events/click/detail", true},

		// Edge cases
		{"", "", true},
		{"*", "x", true},
		{"*", "", false},
		{"**", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"→"+tt.topic, func(t *testing.T) {
			pSegs := splitSegments(tt.pattern)
			tSegs := splitSegments(tt.topic)
			got := matchGlob(pSegs, tSegs)
			if got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.topic, got, tt.want)
			}
		})
	}
}

// splitSegments splits on "/" but handles the empty-string edge case.
func splitSegments(s string) []string {
	if s == "" {
		return []string{""}
	}
	return split(s)
}

func split(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// --- SubscribeGlob integration tests ---

func TestSubscribeGlob_SingleLevelWildcard(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/dashboard/*")
	defer unsub()

	b.Publish("app/dashboard/metrics", "cpu=42")
	b.Publish("app/dashboard/alerts", "warn")
	b.Publish("app/dashboard/metrics/cpu", "should-not-match")
	b.Publish("app/other/stuff", "should-not-match")

	assertGlobMessage(t, ch, "app/dashboard/metrics", "cpu=42")
	assertGlobMessage(t, ch, "app/dashboard/alerts", "warn")
	assertNoGlobMessage(t, ch)
}

func TestSubscribeGlob_MultiLevelWildcard(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/dashboard/**")
	defer unsub()

	b.Publish("app/dashboard", "root")
	b.Publish("app/dashboard/metrics", "m")
	b.Publish("app/dashboard/metrics/cpu", "c")
	b.Publish("app/other", "no")

	assertGlobMessage(t, ch, "app/dashboard", "root")
	assertGlobMessage(t, ch, "app/dashboard/metrics", "m")
	assertGlobMessage(t, ch, "app/dashboard/metrics/cpu", "c")
	assertNoGlobMessage(t, ch)
}

func TestSubscribeGlob_CrossHierarchy(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("**/alerts/critical")
	defer unsub()

	b.Publish("alerts/critical", "a")
	b.Publish("app/alerts/critical", "b")
	b.Publish("app/dashboard/alerts/critical", "c")
	b.Publish("alerts", "no")
	b.Publish("alerts/info", "no")

	assertGlobMessage(t, ch, "alerts/critical", "a")
	assertGlobMessage(t, ch, "app/alerts/critical", "b")
	assertGlobMessage(t, ch, "app/dashboard/alerts/critical", "c")
	assertNoGlobMessage(t, ch)
}

func TestSubscribeGlob_ExactAndGlobCombined(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Exact subscriber
	exactCh, exactUnsub := b.Subscribe("app/dashboard/metrics")
	defer exactUnsub()

	// Glob subscriber
	globCh, globUnsub := b.SubscribeGlob("app/dashboard/*")
	defer globUnsub()

	b.Publish("app/dashboard/metrics", "data")

	// Both should receive the message.
	select {
	case msg := <-exactCh:
		if msg != "data" {
			t.Errorf("exact subscriber got %q, want %q", msg, "data")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("exact subscriber did not receive message")
	}

	assertGlobMessage(t, globCh, "app/dashboard/metrics", "data")
}

func TestSubscribeGlob_Unsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/**")

	b.Publish("app/test", "before")
	assertGlobMessage(t, ch, "app/test", "before")

	unsub()

	// Channel should be closed after unsubscribe.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after unsubscribe")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after unsubscribe")
	}
}

func TestSubscribeGlob_DoubleUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeGlob("app/**")
	unsub()
	unsub() // should not panic
}

func TestUnsubscribeGlob(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, _ := b.SubscribeGlob("app/**")

	b.Publish("app/test", "before")
	assertGlobMessage(t, ch, "app/test", "before")

	b.UnsubscribeGlob(ch)

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after UnsubscribeGlob")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after UnsubscribeGlob")
	}
}

func TestSubscribeGlobScoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlobScoped("app/orders/*/status", "user:123")
	defer unsub()

	// Scoped publish matching pattern and scope.
	b.PublishTo("app/orders/456/status", "user:123", "shipped")
	assertGlobMessage(t, ch, "app/orders/456/status", "shipped")

	// Scoped publish matching pattern but wrong scope.
	b.PublishTo("app/orders/789/status", "user:456", "pending")
	assertNoGlobMessage(t, ch)

	// Unscoped publish should not reach scoped glob subscriber.
	b.Publish("app/orders/111/status", "unscoped")
	assertNoGlobMessage(t, ch)
}

func TestSubscribeGlob_UnscopedDoesNotReceiveScopedPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/orders/**")
	defer unsub()

	// Scoped publish should not reach unscoped glob subscriber.
	b.PublishTo("app/orders/123/status", "user:123", "scoped-msg")
	assertNoGlobMessage(t, ch)

	// Unscoped publish should reach.
	b.Publish("app/orders/123/status", "unscoped-msg")
	assertGlobMessage(t, ch, "app/orders/123/status", "unscoped-msg")
}

func TestSubscribeGlob_LifecycleHooksFireForConcreteTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var mu sync.Mutex
	var firstTopics, lastTopics []string

	b.OnFirstSubscriber("app/orders/123/status", func(topic string) {
		mu.Lock()
		firstTopics = append(firstTopics, topic)
		mu.Unlock()
	})
	b.OnLastUnsubscribe("app/orders/123/status", func(topic string) {
		mu.Lock()
		lastTopics = append(lastTopics, topic)
		mu.Unlock()
	})

	// Glob subscribe does NOT trigger lifecycle hooks on the concrete topic
	// (the glob subscriber is not subscribed to a concrete topic).
	globCh, globUnsub := b.SubscribeGlob("app/orders/*/status")
	_ = globCh

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(firstTopics) != 0 {
		t.Errorf("glob subscribe should not fire OnFirstSubscriber, got %v", firstTopics)
	}
	mu.Unlock()

	// Exact subscribe should fire the hook.
	exactCh, exactUnsub := b.Subscribe("app/orders/123/status")
	_ = exactCh
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(firstTopics) != 1 || firstTopics[0] != "app/orders/123/status" {
		t.Errorf("expected OnFirstSubscriber for concrete topic, got %v", firstTopics)
	}
	mu.Unlock()

	exactUnsub()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(lastTopics) != 1 || lastTopics[0] != "app/orders/123/status" {
		t.Errorf("expected OnLastUnsubscribe for concrete topic, got %v", lastTopics)
	}
	mu.Unlock()

	globUnsub()
}

func TestSubscribeGlob_BrokerClose(t *testing.T) {
	b := NewSSEBroker()

	ch, _ := b.SubscribeGlob("app/**")

	b.Close()

	// Channel should be closed when broker closes.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected glob channel to be closed on broker close")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("glob channel not closed after broker close")
	}
}

func TestSubscribeGlob_ClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.SubscribeGlob("app/**")
	defer unsub()

	// Should get a closed channel immediately.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel from closed broker")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed from closed broker")
	}
}

func TestSubscribeGlob_MultipleGlobSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeGlob("app/**")
	defer unsub1()
	ch2, unsub2 := b.SubscribeGlob("app/dashboard/*")
	defer unsub2()
	ch3, unsub3 := b.SubscribeGlob("**/metrics")
	defer unsub3()

	b.Publish("app/dashboard/metrics", "data")

	// All three should match.
	assertGlobMessage(t, ch1, "app/dashboard/metrics", "data")
	assertGlobMessage(t, ch2, "app/dashboard/metrics", "data")
	assertGlobMessage(t, ch3, "app/dashboard/metrics", "data")
}

func TestSubscribeGlob_ConcurrentPublish(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(100))
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/**")
	defer unsub()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish("app/test", "msg")
		}()
	}
	wg.Wait()

	count := 0
	for {
		select {
		case <-ch:
			count++
		case <-time.After(50 * time.Millisecond):
			goto done
		}
	}
done:
	if count == 0 {
		t.Fatal("expected at least some messages from concurrent publishes")
	}
}

func TestSubscribeGlob_PublishWithReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeGlob("app/dashboard/*")
	defer unsub()

	b.PublishWithReplay("app/dashboard/metrics", "replayed")

	assertGlobMessage(t, ch, "app/dashboard/metrics", "replayed")
}

// --- helpers ---

func assertGlobMessage(t *testing.T, ch <-chan TopicMessage, wantTopic, wantData string) {
	t.Helper()
	select {
	case msg := <-ch:
		if msg.Topic != wantTopic {
			t.Errorf("got topic %q, want %q", msg.Topic, wantTopic)
		}
		if msg.Data != wantData {
			t.Errorf("got data %q, want %q", msg.Data, wantData)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("timeout waiting for message on topic %q", wantTopic)
	}
}

func assertNoGlobMessage(t *testing.T, ch <-chan TopicMessage) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Errorf("unexpected message: topic=%q data=%q", msg.Topic, msg.Data)
	case <-time.After(50 * time.Millisecond):
		// OK: no message expected.
	}
}
