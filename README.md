# tavern

[![Go Reference](https://pkg.go.dev/badge/github.com/catgoose/tavern.svg)](https://pkg.go.dev/github.com/catgoose/tavern)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

![tavern](https://raw.githubusercontent.com/catgoose/screenshots/main/tavern/tavern.png)

Thread-safe, topic-based pub/sub broker for Server-Sent Events (SSE) in Go.

> A master of the React School visit Grug at cave.
>
> Master say: "but how do you manage state?"
>
> Grug say: "server manage state."
>
> Master say: "but how does the client know when state changes?"
>
> Grug say: "server tell it."
>
> Master say: "but--"
>
> Grug say: "server. tell. it."
>
> -- The Recorded Sayings of Layman Grug, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

Tavern provides a minimal, concurrent-safe message broker that fans out string
messages to subscribers by topic. It sits behind any HTTP handler and pushes
real-time events to browser clients over SSE. No JavaScript framework required.

For practical patterns and integration examples, see the
[Recipe Cookbook](RECIPES.md).

---

## Where Tavern Shines

Tavern is general-purpose pub/sub plumbing, but some patterns fall out of it so
naturally that they deserve a callout.

**SaaS Notifications** -- Scoped subscriptions + filters + TTL + replay + OOB
fragments = complete real-time notification system. Per-user streams, org-wide
broadcasts, toast auto-expiry, reconnection recovery. Wire it up to your
existing auth middleware and you have per-tenant push notifications without a
third-party service.

**Live Dashboards** -- Snapshot+delta streams, scheduled publisher with circuit
breakers, adaptive backpressure for mixed client speeds, enhanced observability
for monitoring the monitor. This is what tavern was built for.

**Sports/Event Scoreboards** -- Topic groups for single-connection multi-game
views, hierarchical topics for league/team filtering, gap detection for seamless
reconnection, batch publish for atomic multi-region updates.

**E-commerce Real-time** -- TTL for flash banners and cart timers, batch publish
for inventory+price+availability in one flush, presence for "X people viewing,"
middleware for audit trails.

**HTMX Server-Driven UI** -- Tavern's home turf. OOB fragment swaps, lazy
rendering that skips work when nobody's watching, templ component integration,
mutation hooks that decouple handlers from SSE updates. The server owns the
state, HTML goes over the wire.

**Multi-Instance Deployment** -- Pluggable backend interface, memory backend for
testing, scope-aware message envelopes. Publish on instance A, subscribers on
instance B get it.

---

## Install

```bash
go get github.com/catgoose/tavern
```

## Quick start

```go
broker := tavern.NewSSEBroker()
defer broker.Close()

ch, unsub := broker.Subscribe("events")
defer unsub()

broker.Publish("events", tavern.NewSSEMessage("update", `{"id":1}`).String())

for msg := range ch {
    // handle msg
}
```

Wire it up to an HTTP handler (works with any router):

```go
// One line -- sets SSE headers, handles Last-Event-ID, streams with flush
mux.Handle("/sse/events", broker.SSEHandler("events"))
```

Or the manual way (Echo shown):

```go
func sseHandler(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        ch, unsub := broker.Subscribe("events")
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                if _, err := fmt.Fprint(c.Response(), msg); err != nil {
                    return nil
                }
                c.Response().Flush()
            case <-c.Request().Context().Done():
                return nil
            }
        }
    }
}
```

Override the built-in handler's write step for custom formatting:

```go
mux.Handle("/sse", broker.SSEHandler("events",
    tavern.WithSSEWriter(func(w http.ResponseWriter, msg string) error {
        return myCustomWrite(w, msg)
    }),
))
```

---

## Core pub/sub

> The server sends a representation. The representation contains links and
> forms. The client follows them. THAT IS THE ENTIRE INTERACTION MODEL.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

The server speaks; the client listens. This is the natural order.

### Subscribe / Publish / Unsubscribe / Close

```go
ch, unsub := broker.Subscribe("events")
defer unsub()

broker.Publish("events", "hello, world")
broker.Close() // closes all channels, removes all topics
```

`Publish` fans out to every subscriber. Non-blocking -- if a subscriber's buffer
is full, the message is silently dropped for that subscriber.

### Scoped subscriptions (PublishTo)

Per-user, per-tenant, or per-resource message delivery:

```go
ch, unsub := broker.SubscribeScoped("notifications", userID)
defer unsub()

broker.PublishTo("notifications", userID, msg)
broker.PublishOOBTo("notifications", userID, tavern.Replace("badge", `<span>3</span>`))
```

Scoped and unscoped subscribers are fully independent. `Publish` delivers only
to unscoped; `PublishTo` delivers only to matching scoped subscribers.

### Multiplexed subscriptions (SubscribeMulti)

Subscribe to multiple topics on a single channel, eliminating `reflect.Select`:

```go
ch, unsub := broker.SubscribeMulti("network", "services", "alerts")
defer unsub()

for msg := range ch {
    sse := tavern.NewSSEMessage(msg.Topic, msg.Data).String()
    fmt.Fprint(w, sse)
}
```

### Hierarchical topics with glob wildcards (SubscribeGlob)

Pattern-based subscriptions across topic hierarchies. Topics use `/` as the
separator; `*` matches one segment, `**` matches zero or more:

```go
// All services under monitoring
ch, unsub := broker.SubscribeGlob("monitoring/services/*")
defer unsub()

// Everything under monitoring at any depth
ch, unsub := broker.SubscribeGlob("monitoring/**")
defer unsub()
```

Messages arrive as `TopicMessage` values tagged with the actual publish topic.

---

## Publishing variants

> Hypertext is the simultaneous presentation of information and controls such
> that the information BECOMES THE AFFORDANCE through which choices are obtained
> and actions are selected.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

### PublishWithReplay / PublishWithID / SubscribeFromID

Cache recent messages so new subscribers get them on connect:

```go
broker.SetReplayPolicy("activity", 10) // keep last 10
broker.PublishWithReplay("activity", msg)
```

Track message IDs for gap-free reconnection:

```go
broker.PublishWithID("events", "evt-42", msg)

// On reconnect, browser sends Last-Event-ID -- replay only missed messages
ch, unsub := broker.SubscribeFromID("events", lastEventID)
```

### PublishIfChanged

Content-based deduplication using FNV-64a hashing. Only publishes when the
message actually differs:

```go
broker.PublishIfChanged("dashboard", renderDashboard())
```

### PublishDebounced / PublishThrottled

```go
// Wait for 200ms of quiet, then publish the final value
broker.PublishDebounced("search-results", html, 200*time.Millisecond)

// At most once per second, first call immediate
broker.PublishThrottled("live-stats", html, time.Second)
```

### PublishWithTTL

Ephemeral messages that auto-expire from the replay cache. Current subscribers
get them immediately; new subscribers only see them if the TTL hasn't elapsed:

```go
// Toast notification that expires in 5 seconds
broker.PublishWithTTL("toasts", toastHTML, 5*time.Second,
    tavern.WithAutoRemove("toast-42"), // sends OOB delete on expiry
)
```

Also available: `PublishOOBWithTTL`, `PublishToWithTTL`, `PublishIfChangedWithTTL`.

### Batch publishing (Batch / Flush)

Buffer multiple publishes and deliver them as a single write per subscriber:

```go
batch := broker.Batch()
batch.PublishOOB("dashboard", tavern.Replace("stats", statsHTML))
batch.PublishOOB("dashboard", tavern.Replace("chart", chartHTML))
batch.PublishOOB("dashboard", tavern.Replace("activity", feedHTML))
batch.Flush() // one atomic write per subscriber
```

---

## OOB (out-of-band) fragments

> The whole point -- the ENTIRE POINT -- of hypermedia is that the server tells
> the client what to do next IN THE RESPONSE ITSELF.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

OOB swaps are SSE's answer to this. The server sends the exact DOM mutations
to apply:

```go
broker.PublishOOB("events",
    tavern.Replace("stats-bar", "<span>42</span>"),
    tavern.Delete("task-row-5"),
    tavern.Append("activity-feed", "<li>New item</li>"),
    tavern.Prepend("alert-list", "<li>Alert!</li>"),
)
```

### Component interface (templ integration)

`Component` renders itself to a writer. The interface matches `templ.Component`
exactly -- pass templ components directly, no imports needed:

```go
broker.PublishOOB("events",
    tavern.ReplaceComponent("stats-bar", views.StatsBar(stats)),
    tavern.AppendComponent("feed", views.FeedItem(item)),
)
```

If rendering fails, the fragment contains an HTML comment with the error
rather than a partial render.

### Lazy rendering (PublishLazyOOB)

Skip expensive rendering when nobody is listening:

```go
broker.PublishLazyOOB("dashboard", func() []tavern.Fragment {
    stats := fetchStats(db) // only runs if someone is subscribed
    return []tavern.Fragment{
        tavern.ReplaceComponent("stats", views.StatsPanel(stats)),
    }
})

// With deduplication
broker.PublishLazyIfChangedOOB("dashboard", func() []tavern.Fragment { ... })
```

### PublishOOBWithTTL

Ephemeral OOB fragments:

```go
broker.PublishOOBWithTTL("toasts", 5*time.Second,
    tavern.Replace("toast-area", toastHTML),
)
```

---

## SSE handlers

### SSEHandler

The built-in handler sets SSE headers, handles `Last-Event-ID` resumption, and
streams messages with flush:

```go
mux.Handle("/sse/events", broker.SSEHandler("events"))
```

### Topic groups (GroupHandler / DynamicGroupHandler)

Serve multiple topics on a single SSE connection:

```go
// Static group -- same topics for everyone
broker.DefineGroup("dashboard", []string{"stats", "alerts", "activity"})
mux.Handle("/sse/dashboard", broker.GroupHandler("dashboard"))

// Dynamic group -- per-request topic resolution (authorization, etc.)
broker.DynamicGroup("user-dashboard", func(r *http.Request) []string {
    user := auth.FromContext(r.Context())
    return topicsForRole(user.Role)
})
mux.Handle("/sse/user", broker.DynamicGroupHandler("user-dashboard"))
```

### Snapshot + delta (SubscribeWithSnapshot)

Send a computed snapshot as the first message, then live updates. Eliminates
the dual-render pattern:

```go
ch, unsub := broker.SubscribeWithSnapshot("dashboard", func() string {
    return renderFullDashboard()
})
defer unsub()
// First message is the snapshot, then live publishes follow
```

---

## Subscriber management

### Metadata (SubscribeWithMeta)

Tag subscribers for admin panels and debugging:

```go
ch, unsub := broker.SubscribeWithMeta("dashboard", tavern.SubscribeMeta{
    ID:   sessionID,
    Meta: map[string]string{"user": userName, "addr": remoteAddr},
})
defer unsub()

subs := broker.Subscribers("dashboard")
broker.Disconnect("dashboard", sessionID) // force disconnect
```

### Subscriber filtering (SubscribeWithFilter)

Per-subscriber message filtering in the publish path:

```go
ch, unsub := broker.SubscribeWithFilter("activity", func(msg string) bool {
    return strings.Contains(msg, userID) // only this user's activity
})
defer unsub()
```

Non-matching messages are silently skipped without counting toward drops or
backpressure.

### Per-subscriber rate limiting (SubscribeWithRate)

```go
ch, unsub := broker.SubscribeWithRate("live-data", tavern.Rate{
    MaxPerSecond: 5, // at most 5 msg/s to this subscriber
})
defer unsub()
```

Messages faster than the rate are held; the most recent held message is
delivered when the interval elapses (latest-wins). Does not affect other
subscribers.

### Server-initiated subscription changes (AddTopic / RemoveTopic)

Dynamically modify a subscriber's topic set without reconnecting:

```go
// Add a topic -- subscriber starts receiving it immediately
broker.AddTopic(subscriberID, "new-topic", true) // true = send control event

// Remove a topic
broker.RemoveTopic(subscriberID, "old-topic", true)

// Scope-wide changes
broker.AddTopicForScope("admin", "audit-log", true)
```

A `tavern-topics-changed` control event notifies the client so it can set up
new SSE-swap targets.

### Connection events (WithConnectionEvents)

Publish subscribe/unsubscribe as SSE events on a meta topic:

```go
broker := tavern.NewSSEBroker(tavern.WithConnectionEvents("_meta"))

ch, unsub := broker.Subscribe("_meta")
// Receives: {"event":"subscribe","topic":"dashboard","subscribers":3}
// Receives: {"event":"unsubscribe","topic":"dashboard","subscribers":2}
```

The meta topic does not generate recursive events for its own subscribers.

---

## Reactive hooks

### After hooks (topic dependencies)

Fire callbacks after a successful publish to chain dependent updates:

```go
broker.After("orders", func() {
    broker.PublishOOB("dashboard",
        tavern.ReplaceComponent("order-count", views.OrderCount(db)),
    )
})
```

Hooks run asynchronously in a new goroutine. Cycle detection prevents infinite
loops (max depth 8, skips already-visited topics in the chain).

### OnMutate / NotifyMutate

Decouple mutation signals from specific topics. Register handlers for logical
resources, trigger them from your business logic:

```go
broker.OnMutate("orders", func(evt tavern.MutationEvent) {
    order := evt.Data.(*Order)
    broker.PublishOOB("order-detail",
        tavern.ReplaceComponent("order-"+order.ID, views.OrderRow(order)),
    )
    broker.PublishOOB("dashboard",
        tavern.ReplaceComponent("order-stats", views.OrderStats(db)),
    )
})

// In your handler:
broker.NotifyMutate("orders", tavern.MutationEvent{ID: orderID, Data: order})
```

### Publish middleware (Use / UseTopics)

Intercept, transform, or swallow publishes:

```go
// Global middleware -- runs on every publish
broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
    return func(topic, msg string) {
        slog.Info("publish", "topic", topic, "size", len(msg))
        next(topic, msg)
    }
})

// Topic-scoped -- wildcards with ":" separator
broker.UseTopics("admin:*", func(next tavern.PublishFunc) tavern.PublishFunc {
    return func(topic, msg string) {
        auditLog(topic, msg)
        next(topic, msg)
    }
})
```

---

## Reconnection and resilience

### Replay gap detection (OnReplayGap / SetReplayGapPolicy)

Handle reconnections where the client's Last-Event-ID has rolled out of the
replay log:

```go
broker.OnReplayGap("dashboard", func(sub *tavern.SubscriberInfo, lastID string) {
    slog.Warn("replay gap", "subscriber", sub.ID, "lastID", lastID)
})

// Fall back to a full snapshot when a gap is detected
broker.SetReplayGapPolicy("dashboard", tavern.GapFallbackToSnapshot, func() string {
    return renderFullDashboard()
})
```

### Reconnection UX (OnReconnect / BundleOnReconnect)

```go
broker.OnReconnect("dashboard", func(info tavern.ReconnectInfo) {
    slog.Info("reconnect", "topic", info.Topic, "gap", info.Gap, "missed", info.MissedCount)
    // Send a welcome-back message directly to this subscriber
    info.SendToSubscriber(tavern.NewSSEMessage("reconnected", "welcome back").String())
})

// Bundle replay messages into a single write to reduce DOM churn
broker.SetBundleOnReconnect("dashboard", true)
```

### Adaptive backpressure

Tiered response to slow subscribers -- throttle, simplify, then disconnect:

```go
broker := tavern.NewSSEBroker(
    tavern.WithAdaptiveBackpressure(tavern.AdaptiveBackpressure{
        ThrottleAt:   5,   // deliver every 2nd message
        SimplifyAt:   20,  // apply simplified renderer
        DisconnectAt: 50,  // evict the subscriber
    }),
)

// Register a lightweight renderer for the simplify tier
broker.SetSimplifiedRenderer("dashboard", func(msg string) string {
    return `<div id="dashboard">Loading...</div>`
})

// Get notified on tier changes
broker.OnBackpressureTierChange(func(sub *tavern.SubscriberInfo, old, new tavern.BackpressureTier) {
    slog.Warn("backpressure", "subscriber", sub.ID, "old", old, "new", new)
})
```

### Slow subscriber eviction

Simple threshold-based eviction without the full adaptive tier system:

```go
broker := tavern.NewSSEBroker(
    tavern.WithSlowSubscriberEviction(100),
    tavern.WithSlowSubscriberCallback(func(topic string) {
        slog.Warn("slow subscriber evicted", "topic", topic)
    }),
)
```

---

## Error handling

### OnRenderError callback

Centralized error handling for render failures in scheduled publishers:

```go
broker.OnRenderError(func(err *tavern.RenderError) {
    slog.Error("render failed",
        "topic", err.Topic,
        "section", err.Section,
        "error", err.Err,
        "count", err.Count,
    )
})
```

### Circuit breaker for ScheduledPublisher

Protect scheduled sections from cascading failures:

```go
pub.Register("services", 3*time.Second, renderServices, tavern.SectionOptions{
    CircuitBreaker: &tavern.CircuitBreakerConfig{
        FailureThreshold: 3,
        RecoveryInterval: 30 * time.Second,
        FallbackRender: func() string {
            return `<div id="services">Service data temporarily unavailable</div>`
        },
    },
})
```

After 3 consecutive failures, the circuit opens and renders the fallback. After
30 seconds, a probe request tests recovery.

---

## Scheduled publishing

`ScheduledPublisher` manages multiple named sections with independent
intervals. It ticks on a fast base interval, renders due sections into a
shared buffer, and publishes one batched message per tick. Skips rendering
when no subscribers are listening.

```go
pub := broker.NewScheduledPublisher("dashboard", tavern.WithBaseTick(100*time.Millisecond))

pub.Register("network", 1*time.Second, func(ctx context.Context, buf *bytes.Buffer) error {
    return views.NetworkChart(snap).Render(ctx, buf)
})
pub.Register("services", 3*time.Second, func(ctx context.Context, buf *bytes.Buffer) error {
    return views.ServicesPanel(services).Render(ctx, buf)
})

broker.RunPublisher(ctx, pub.Start)

// Runtime interval changes
pub.SetInterval("network", 500*time.Millisecond)
```

`RunPublisher` launches a publisher goroutine with panic recovery, tracked by
the broker's WaitGroup so `Close()` waits for all publishers to return.

---

## Observability

### Basic stats

```go
if broker.HasSubscribers("system-stats") {
    broker.Publish("system-stats", renderStats())
}

counts := broker.TopicCounts()           // map[string]int
total := broker.SubscriberCount()        // int
drops := broker.PublishDrops()           // int64

s := broker.Stats()
// BrokerStats{Topics: int, Subscribers: int, PublishDrops: int64}
```

### Per-topic metrics (WithMetrics)

Opt-in publish and drop counters per topic:

```go
broker := tavern.NewSSEBroker(tavern.WithMetrics())

m := broker.Metrics()
for topic, stats := range m.TopicStats {
    fmt.Printf("%s: published=%d dropped=%d peak_subs=%d\n",
        topic, stats.Published, stats.Dropped, stats.PeakSubscribers)
}
```

### Enhanced observability (WithObservability)

Latency histograms, subscriber lag, throughput, and connection durations:

```go
broker := tavern.NewSSEBroker(tavern.WithObservability(tavern.ObservabilityConfig{
    PublishLatency:     true,
    SubscriberLag:      true,
    ConnectionDuration: true,
    TopicThroughput:    true,
}))

obs := broker.Observability()
p99 := obs.PublishLatencyP99("dashboard")
lag := obs.SubscriberLag("dashboard", broker)
rate := obs.TopicThroughput("dashboard")
snap := obs.Snapshot(broker) // all topics at once
```

Zero overhead when not configured.

---

## Testing

The `taverntest` subpackage provides test helpers:

```go
import "github.com/catgoose/tavern/taverntest"

// Recorder -- subscribe and collect messages
rec := taverntest.NewRecorder(broker, "events")
defer rec.Close()
rec.WaitFor(3, time.Second)
rec.AssertCount(t, 3)
rec.AssertContains(t, "expected-message")

// Capture -- declarative assertions
cap := taverntest.NewCapture(broker, "events")
defer cap.Close()
cap.WaitFor(2, time.Second)
cap.AssertMessages(t, "first", "second")

// MockBroker -- record publishes without a real broker
mock := taverntest.NewMockBroker()
mock.Publish("events", "msg")
mock.AssertPublished(t, "events", "msg")

// SlowSubscriber -- test backpressure and eviction
slow := taverntest.NewSlowSubscriber(broker, "events", taverntest.SlowSubscriberConfig{
    ReadDelay: 100 * time.Millisecond,
})
defer slow.Close()

// SimulatedConnection -- test reconnection and Last-Event-ID
conn := taverntest.NewSimulatedConnection(broker, "events")
conn.Disconnect()
conn.Reconnect()
conn.AssertReconnectMessages(t, ...)

// SSERecorder -- capture SSE wire output for handler tests
rec := taverntest.NewSSERecorder()
handler.ServeHTTP(rec, req)
rec.AssertEventCount(t, 3)
rec.AssertEvent(t, 0, taverntest.SSEEvent{Event: "update", Data: "hello"})
```

---

## Subpackages

### presence/ -- Structured presence tracking

Heartbeat-based presence with stale detection and optional OOB publishing:

```go
import "github.com/catgoose/tavern/presence"

tracker := presence.New(broker, presence.Config{
    StaleTimeout: 30 * time.Second,
    RenderFunc: func(topic string, users []presence.Info) string {
        return renderPresenceList(users)
    },
    OnJoin:  func(topic string, info presence.Info) { /* ... */ },
    OnLeave: func(topic string, info presence.Info) { /* ... */ },
})
defer tracker.Close()

tracker.Join("doc-123", presence.Info{UserID: userID, Name: userName})
tracker.Heartbeat("doc-123", userID)
tracker.Update("doc-123", userID, map[string]any{"cursor": pos})
tracker.Leave("doc-123", userID)

users := tracker.List("doc-123")
```

### backend/ -- Distributed fan-out interface

The `backend.Backend` interface enables cross-process fan-out. Publishes on one
broker instance reach subscribers on another.

### backend/memory/ -- In-process backend for testing

Simulate multi-instance deployments in tests:

```go
import "github.com/catgoose/tavern/backend/memory"

mem := memory.New()
fork := mem.Fork() // shares the same message bus

broker1 := tavern.NewSSEBroker(tavern.WithBackend(mem))
broker2 := tavern.NewSSEBroker(tavern.WithBackend(fork))

// publish on broker1, subscribers on broker2 receive it
```

---

## Configuration

`NewSSEBroker` accepts functional options:

| Option | Default | Description |
|--------|---------|-------------|
| `WithBufferSize(n)` | 10 | Subscriber channel buffer capacity |
| `WithDropOldest()` | drop newest | Discard oldest queued message when buffer full |
| `WithKeepalive(d)` | disabled | Send SSE comment keepalives at interval |
| `WithTopicTTL(d)` | disabled | Auto-remove topics with no subscribers after TTL |
| `WithSlowSubscriberEviction(n)` | disabled | Evict after n consecutive drops |
| `WithAdaptiveBackpressure(cfg)` | disabled | Tiered backpressure (throttle/simplify/disconnect) |
| `WithMetrics()` | disabled | Per-topic publish/drop counters |
| `WithObservability(cfg)` | disabled | Latency, lag, throughput, connection duration |
| `WithConnectionEvents(topic)` | disabled | Publish subscribe/unsubscribe events |
| `WithLogger(l)` | nil | Log panics and errors via slog |
| `WithBackend(b)` | nil | Cross-process fan-out backend |

Additional runtime configuration:

```go
broker.SetReplayPolicy("topic", 10)        // replay cache size
broker.SetRetry("topic", 30*time.Second)   // client reconnect delay
broker.SetRetryAll(30*time.Second)          // all topics
broker.OnRenderError(func(err *tavern.RenderError) { ... })
```

---

## SSE message format

```go
msg := tavern.NewSSEMessage("update", `{"id":1}`).String()
// event: update\ndata: {"id":1}\n\n

msg := tavern.NewSSEMessage("update", data).WithID("42").WithRetry(5000).String()
// event: update\ndata: ...\nid: 42\nretry: 5000\n\n
```

---

## Thread safety

All `SSEBroker` methods are safe for concurrent use. The broker uses
`sync.RWMutex` internally: subscribing and unsubscribing take a write lock,
publishing and reading counts take a read lock. `Publish` snapshots the
subscriber set under the read lock, then sends outside it, so publishers never
block each other.

---

## Philosophy

Tavern follows the [dothog design philosophy](https://github.com/catgoose/dothog/blob/main/PHILOSOPHY.md)
and the [Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md):
the server drives state, the broker is just plumbing, and `sync.RWMutex` is
the only dependency you need for thread safety.

> wife of Grug say from cave: "easy, easy, easy. like touching feet to ground
> when get out of bed. server return html. browser render html. what is
> difficult?"
>
> -- The Recorded Sayings of Layman Grug, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

Server publish event. Browser receive event. What is difficult?

> SSE is the server telling the client what happened next, in real time. The
> event stream is just another representation -- the server speaks, the client
> listens, and nobody had to install an npm package to make it work.

---

## Architecture

```
  handler --> broker.Publish("topic", msg)
                      |
                      +---> subscriber A (chan) ---> SSE endpoint ---> browser A
                      +---> subscriber B (chan) ---> SSE endpoint ---> browser B
                      +---> subscriber C (chan) ---> SSE endpoint ---> browser C
```

---

## License

[MIT](LICENSE)
