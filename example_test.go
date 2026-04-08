package tavern_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/presence"
)

// Topic name conventions for dashboard and real-time UI applications. Your
// application may use any string as a topic name; these are provided as
// examples of consistent naming patterns.
const (
	TopicSystemStats  = "system-stats"
	TopicDashMetrics  = "dashboard-metrics"
	TopicDashServices = "dashboard-services"
	TopicDashEvents   = "dashboard-events"
	TopicPeopleUpdate = "people-update"
	TopicActivityFeed = "activity-feed"
	TopicErrorTraces  = "error-traces"
	TopicThemeChange  = "theme-change"
	TopicCanvasUpdate = "canvas-update"
)

func ExampleSSEBroker_pubsub() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe(TopicSystemStats)
	defer unsub()

	broker.Publish(TopicSystemStats, `{"cpu": 42}`)

	msg := <-ch
	fmt.Println(msg)
	// Output: {"cpu": 42}
}

func ExampleSSEBroker_Stats() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	_, unsub1 := broker.Subscribe(TopicSystemStats)
	defer unsub1()
	_, unsub2 := broker.Subscribe(TopicActivityFeed)
	defer unsub2()

	stats := broker.Stats()
	fmt.Printf("topics=%d subscribers=%d drops=%d\n", stats.Topics, stats.Subscribers, stats.PublishDrops)
	// Output: topics=2 subscribers=2 drops=0
}

// ExampleSSEBroker_GroupHandler demonstrates a static topic group that
// multiplexes several topics onto a single SSE connection. This is useful
// when a dashboard page needs data from multiple topics without opening
// separate EventSource connections.
func ExampleSSEBroker_GroupHandler() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.DefineGroup("dashboard", []string{"metrics", "alerts", "status"})

	handler := broker.GroupHandler("dashboard")
	mux := http.NewServeMux()
	mux.Handle("/sse/dashboard", handler)

	fmt.Println("handler registered")
	// Output: handler registered
}

// ExampleSSEBroker_DynamicGroupHandler demonstrates a dynamic topic group
// where the topics are resolved per-request. This enables per-user
// authorization so different users receive different topic sets from the
// same endpoint.
func ExampleSSEBroker_DynamicGroupHandler() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.DynamicGroup("user-feed", func(r *http.Request) []string {
		userID := r.URL.Query().Get("user")
		return []string{"global-feed", "user/" + userID + "/notifications"}
	})

	handler := broker.DynamicGroupHandler("user-feed")
	mux := http.NewServeMux()
	mux.Handle("/sse/feed", handler)

	fmt.Println("dynamic handler registered")
	// Output: dynamic handler registered
}

// ExampleSSEBroker_SubscribeGlob demonstrates hierarchical topic patterns
// using glob subscriptions. The "*" wildcard matches a single segment and
// "**" matches zero or more segments.
func ExampleSSEBroker_SubscribeGlob() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	// Subscribe to all topics under "sensors/" with one level of nesting.
	ch, unsub := broker.SubscribeGlob("sensors/*")
	defer unsub()

	broker.Publish("sensors/temperature", `{"value": 22.5}`)
	broker.Publish("sensors/humidity", `{"value": 60}`)
	// This won't match because "sensors/floor/1" has two segments after "sensors".
	broker.Publish("sensors/floor/1", `{"value": 3}`)

	msg1 := <-ch
	msg2 := <-ch
	fmt.Printf("topic=%s data=%s\n", msg1.Topic, msg1.Data)
	fmt.Printf("topic=%s data=%s\n", msg2.Topic, msg2.Data)
	// Output:
	// topic=sensors/temperature data={"value": 22.5}
	// topic=sensors/humidity data={"value": 60}
}

// ExampleSSEBroker_PublishWithID demonstrates publishing messages with event
// IDs for Last-Event-ID resumption. When a client reconnects, it can resume
// from where it left off using SubscribeFromID.
func ExampleSSEBroker_PublishWithID() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.SetReplayPolicy("orders", 10)

	ch, unsub := broker.Subscribe("orders")
	defer unsub()

	broker.PublishWithID("orders", "evt-1", `{"order": "A"}`)

	// Live subscriber receives the message with the injected id: field.
	msg := <-ch
	fmt.Println(extractData(msg))
	// Output: {"order": "A"}
}

// ExampleSSEBroker_SubscribeFromID demonstrates replaying cached messages
// for a new subscriber when no Last-Event-ID is provided. When lastEventID
// is empty, all cached replay messages are delivered.
func ExampleSSEBroker_SubscribeFromID() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.SetReplayPolicy("chat", 5)

	broker.PublishWithID("chat", "msg-1", "hello")
	broker.PublishWithID("chat", "msg-2", "world")

	// Empty lastEventID replays all cached messages.
	ch, unsub := broker.SubscribeFromID("chat", "")
	defer unsub()

	m1 := <-ch
	m2 := <-ch
	fmt.Println(extractData(m1))
	fmt.Println(extractData(m2))
	// Output:
	// hello
	// world
}

// ExampleSSEBroker_Batch demonstrates atomic multi-message publishing. All
// messages in a batch are concatenated per topic and delivered as a single
// channel write, reducing SSE frame overhead.
func ExampleSSEBroker_Batch() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe("updates")
	defer unsub()

	batch := broker.Batch()
	batch.Publish("updates", "line1\n")
	batch.Publish("updates", "line2\n")
	batch.Flush()

	// The subscriber receives all batched messages as one concatenated write.
	msg := <-ch
	fmt.Print(msg)
	// Output:
	// line1
	// line2
}

// ExamplePublishBatch_Discard demonstrates discarding a batch without
// sending any messages.
func ExamplePublishBatch_Discard() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe("updates")
	defer unsub()

	batch := broker.Batch()
	batch.Publish("updates", "draft message")
	batch.Discard()

	// Verify nothing was delivered by publishing a sentinel.
	broker.Publish("updates", "after-discard")
	msg := <-ch
	fmt.Println(msg)
	// Output: after-discard
}

// ExampleSSEBroker_PublishWithTTL demonstrates ephemeral messages that
// expire from the replay cache after a duration. Current subscribers
// receive the message immediately, but new subscribers who connect after
// the TTL elapses will not see it.
func ExampleSSEBroker_PublishWithTTL() {
	broker := tavern.NewSSEBroker(tavern.WithMessageTTLSweep(10 * time.Millisecond))
	defer broker.Close()

	broker.SetReplayPolicy("alerts", 10)

	ch, unsub := broker.Subscribe("alerts")
	defer unsub()

	broker.PublishWithTTL("alerts", "temporary alert", 50*time.Millisecond)

	msg := <-ch
	fmt.Println(msg)
	// Output: temporary alert
}

// ExampleWithAutoRemove demonstrates using WithAutoRemove to automatically
// send an OOB delete fragment when a TTL message expires.
func ExampleWithAutoRemove() {
	broker := tavern.NewSSEBroker(tavern.WithMessageTTLSweep(10 * time.Millisecond))
	defer broker.Close()

	broker.SetReplayPolicy("toasts", 10)

	ch, unsub := broker.Subscribe("toasts")
	defer unsub()

	broker.PublishWithTTL("toasts", "<div id=\"toast-1\">Notice</div>", 30*time.Millisecond,
		tavern.WithAutoRemove("toast-1"),
	)

	msg := <-ch
	fmt.Println(msg)
	// Output: <div id="toast-1">Notice</div>
}

// ExampleSSEBroker_After demonstrates After hooks that fire asynchronously
// after a successful publish. This is useful for triggering side effects
// like cache invalidation or dependent topic updates.
func ExampleSSEBroker_After() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	var done sync.WaitGroup
	done.Add(1)

	ch, unsub := broker.Subscribe("audit-log")
	defer unsub()

	broker.After("orders", func() {
		broker.Publish("audit-log", "order topic updated")
		done.Done()
	})

	broker.Publish("orders", `{"id": 1}`)
	done.Wait()

	msg := <-ch
	fmt.Println(msg)
	// Output: order topic updated
}

// ExampleSSEBroker_OnMutate demonstrates the OnMutate/NotifyMutate pattern
// for decoupling resource mutations from topic updates.
func ExampleSSEBroker_OnMutate() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe("order-updates")
	defer unsub()

	broker.OnMutate("orders", func(e tavern.MutationEvent) {
		broker.Publish("order-updates", fmt.Sprintf("order %s changed", e.ID))
	})

	broker.NotifyMutate("orders", tavern.MutationEvent{ID: "42", Data: "shipped"})

	msg := <-ch
	fmt.Println(msg)
	// Output: order 42 changed
}

// ExampleSSEBroker_Use demonstrates global publish middleware. Middleware
// wraps every publish call and can transform messages, add logging, or
// swallow publishes entirely.
func ExampleSSEBroker_Use() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	// Add a middleware that uppercases all messages.
	broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			next(topic, strings.ToUpper(msg))
		}
	})

	ch, unsub := broker.Subscribe("events")
	defer unsub()

	broker.Publish("events", "hello world")

	msg := <-ch
	fmt.Println(msg)
	// Output: HELLO WORLD
}

// ExampleSSEBroker_UseTopics demonstrates topic-scoped middleware that
// only runs when the publish topic matches a pattern.
func ExampleSSEBroker_UseTopics() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	// Add middleware only for topics matching "log:*".
	broker.UseTopics("log:*", func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			next(topic, "[LOG] "+msg)
		}
	})

	logCh, unsub1 := broker.Subscribe("log:app")
	defer unsub1()
	dataCh, unsub2 := broker.Subscribe("data")
	defer unsub2()

	broker.Publish("log:app", "request handled")
	broker.Publish("data", "raw value")

	fmt.Println(<-logCh)
	fmt.Println(<-dataCh)
	// Output:
	// [LOG] request handled
	// raw value
}

// ExampleTracker demonstrates the presence package for tracking user
// join/leave lifecycle on topics.
func ExampleTracker() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	var events []string
	var mu sync.Mutex

	tracker := presence.New(broker, presence.Config{
		StaleTimeout: time.Minute,
		OnJoin: func(topic string, info presence.Info) {
			mu.Lock()
			events = append(events, fmt.Sprintf("join:%s:%s", topic, info.UserID))
			mu.Unlock()
		},
		OnLeave: func(topic string, info presence.Info) {
			mu.Lock()
			events = append(events, fmt.Sprintf("leave:%s:%s", topic, info.UserID))
			mu.Unlock()
		},
	})
	defer tracker.Close()

	tracker.Join("room-1", presence.Info{UserID: "alice", Name: "Alice"})
	tracker.Join("room-1", presence.Info{UserID: "bob", Name: "Bob"})

	users := tracker.List("room-1")
	// Sort for deterministic output.
	sort.Slice(users, func(i, j int) bool { return users[i].UserID < users[j].UserID })
	for _, u := range users {
		fmt.Printf("present: %s (%s)\n", u.UserID, u.Name)
	}

	tracker.Leave("room-1", "bob")

	mu.Lock()
	for _, e := range events {
		fmt.Println(e)
	}
	mu.Unlock()
	// Output:
	// present: alice (Alice)
	// present: bob (Bob)
	// join:room-1:alice
	// join:room-1:bob
	// leave:room-1:bob
}

// ExampleSSEBroker_PublishDebounced demonstrates debounced publishing where
// only the last message in a rapid sequence is delivered after a quiet period.
func ExampleSSEBroker_PublishDebounced() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe("search")
	defer unsub()

	// Simulate rapid typing — only the final value should be published.
	broker.PublishDebounced("search", "h", 50*time.Millisecond)
	broker.PublishDebounced("search", "he", 50*time.Millisecond)
	broker.PublishDebounced("search", "hel", 50*time.Millisecond)
	broker.PublishDebounced("search", "hello", 50*time.Millisecond)

	msg := <-ch
	fmt.Println(msg)
	// Output: hello
}

// ExampleSSEBroker_PublishThrottled demonstrates throttled publishing where
// the first message publishes immediately and subsequent messages within the
// interval are rate-limited.
func ExampleSSEBroker_PublishThrottled() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe("slider")
	defer unsub()

	// First call publishes immediately.
	broker.PublishThrottled("slider", "first", 100*time.Millisecond)

	msg := <-ch
	fmt.Println(msg)
	// Output: first
}

// ExampleWithSSEWriter demonstrates a custom SSE writer that formats
// messages with a prefix before writing them to the HTTP response.
func ExampleWithSSEWriter() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	customWriter := tavern.WithSSEWriter(func(w http.ResponseWriter, msg string) error {
		_, err := fmt.Fprintf(w, "data: %s\n\n", msg)
		return err
	})

	handler := broker.SSEHandler("events", customWriter)
	mux := http.NewServeMux()
	mux.Handle("/sse", handler)

	fmt.Println("custom writer handler registered")
	// Output: custom writer handler registered
}

// ExampleSSEBroker_SubscribeMulti demonstrates subscribing to multiple
// topics with a single channel. Each received message includes the source
// topic, eliminating the need for reflect.Select.
func ExampleSSEBroker_SubscribeMulti() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.SubscribeMulti("cpu", "memory", "disk")
	defer unsub()

	broker.Publish("memory", "85%")
	broker.Publish("cpu", "42%")
	broker.Publish("disk", "67%")

	// Collect all three messages.
	msgs := make([]string, 3)
	for i := 0; i < 3; i++ {
		m := <-ch
		msgs[i] = fmt.Sprintf("%s=%s", m.Topic, m.Data)
	}
	sort.Strings(msgs)
	for _, m := range msgs {
		fmt.Println(m)
	}
	// Output:
	// cpu=42%
	// disk=67%
	// memory=85%
}

// ExampleSSEBroker_OnFirstSubscriber demonstrates lifecycle hooks that fire
// when the first subscriber joins a topic and when the last one leaves.
func ExampleSSEBroker_OnFirstSubscriber() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	var firstDone, lastDone sync.WaitGroup
	firstDone.Add(1)
	lastDone.Add(1)

	broker.OnFirstSubscriber("prices", func(topic string) {
		fmt.Printf("first subscriber on %s\n", topic)
		firstDone.Done()
	})
	broker.OnLastUnsubscribe("prices", func(topic string) {
		fmt.Printf("last subscriber left %s\n", topic)
		lastDone.Done()
	})

	_, unsub := broker.Subscribe("prices")
	firstDone.Wait()

	unsub()
	lastDone.Wait()
	// Output:
	// first subscriber on prices
	// last subscriber left prices
}

// ExampleSSEBroker_SetReplayPolicy demonstrates configuring replay so that
// new subscribers receive recently cached messages on connect.
func ExampleSSEBroker_SetReplayPolicy() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.SetReplayPolicy("news", 3)

	// Publish before any subscriber exists.
	broker.PublishWithReplay("news", "headline-1")
	broker.PublishWithReplay("news", "headline-2")
	broker.PublishWithReplay("news", "headline-3")

	// New subscriber receives the cached messages.
	ch, unsub := broker.Subscribe("news")
	defer unsub()

	for i := 0; i < 3; i++ {
		fmt.Println(<-ch)
	}
	// Output:
	// headline-1
	// headline-2
	// headline-3
}

func ExampleSSEBroker_lifelineHandoff() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	// Lifeline: one persistent connection for app-shell events.
	ch, unsub := broker.SubscribeMultiWithMeta(tavern.SubscribeMeta{ID: "app"}, "control")
	defer unsub()

	// User navigates to dashboard -- add topic dynamically.
	broker.AddTopic("app", "dashboard", true)

	// Control event confirms the topic was added.
	ctrl := <-ch
	fmt.Println("control event topic:", ctrl.Topic)

	// Both topics now deliver through the single lifeline channel.
	broker.Publish("dashboard", "chart-update")
	msg := <-ch
	fmt.Printf("topic=%s data=%s\n", msg.Topic, msg.Data)

	// User navigates away -- remove dashboard topic.
	broker.RemoveTopic("app", "dashboard", true)

	// Drain the removal control event.
	<-ch

	// Output:
	// control event topic: tavern-topics-changed
	// topic=dashboard data=chart-update
}

func ExampleSSEBroker_lifelineFallback() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	// Lifeline carries control-plane and fallback signals.
	ch, unsub := broker.SubscribeMultiWithMeta(tavern.SubscribeMeta{ID: "app"}, "control", "fallback")
	defer unsub()

	// Scoped panel stream -- active while viewing the panel.
	_, panelUnsub := broker.SubscribeScoped("panel-data", "user:1")

	// User navigates away -- panel stream torn down.
	panelUnsub()

	// Lifeline still delivers fallback/invalidation signals.
	broker.Publish("fallback", "data-stale")
	msg := <-ch
	fmt.Printf("topic=%s data=%s\n", msg.Topic, msg.Data)
	// Output:
	// topic=fallback data=data-stale
}

func ExampleSSEBroker_lifelineReplay() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	broker.SetReplayPolicy("panel", 10)

	// Lifeline stays connected throughout.
	lifeline, lifelineUnsub := broker.SubscribeMultiWithMeta(tavern.SubscribeMeta{ID: "app"}, "control")
	defer lifelineUnsub()

	// Publish panel events with IDs while scoped stream is down.
	broker.PublishWithID("panel", "e1", "update-1")
	broker.PublishWithID("panel", "e2", "update-2")

	// Scoped stream reconnects with last known ID -- replay fills the gap.
	panelCh, panelUnsub := broker.SubscribeFromID("panel", "e1")
	defer panelUnsub()

	// Skip the reconnected control event.
	<-panelCh

	// Replayed message arrives.
	replayed := <-panelCh
	fmt.Println("replayed:", extractData(replayed))

	// Lifeline was never interrupted.
	broker.Publish("control", "still-alive")
	msg := <-lifeline
	fmt.Printf("lifeline: topic=%s data=%s\n", msg.Topic, msg.Data)
	// Output:
	// replayed: update-2
	// lifeline: topic=control data=still-alive
}

// extractData is a test helper that extracts the raw data from a message
// that may contain injected SSE id: fields.
func extractData(msg string) string {
	// Messages from SubscribeFromID have "id: <id>\n" prepended.
	// Strip any "id: ..." lines to get the raw data.
	var parts []string
	for _, line := range strings.Split(msg, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			parts = append(parts, line)
		}
	}
	result := strings.Join(parts, "\n")
	return strings.TrimSpace(result)
}
