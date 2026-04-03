# tavern

<!--toc:start-->

- [tavern](#tavern)
  - [Why](#why)
  - [Install](#install)
  - [Quick start](#quick-start)
  - [Core concepts](#core-concepts)
    - [Subscribe and unsubscribe](#subscribe-and-unsubscribe)
    - [Publish](#publish)
    - [Scoped subscriptions](#scoped-subscriptions)
    - [Format SSE messages](#format-sse-messages)
    - [Graceful shutdown](#graceful-shutdown)
  - [Publishing variants](#publishing-variants)
    - [PublishWithReplay](#publishwithreplay)
    - [PublishWithID / SubscribeFromID](#publishwithid--subscribefromid)
    - [PublishIfChanged](#publishifchanged)
    - [PublishDebounced](#publishdebounced)
    - [PublishThrottled](#publishthrottled)
  - [Broker configuration](#broker-configuration)
    - [WithBufferSize](#withbuffersize)
    - [WithDropOldest](#withdropoldest)
    - [WithKeepalive](#withkeepalive)
    - [WithTopicTTL](#withtopicttl)
    - [WithLogger](#withlogger)
    - [SetReplayPolicy](#setreplaypolicy)
  - [Lifecycle hooks](#lifecycle-hooks)
    - [OnFirstSubscriber / OnLastUnsubscribe](#onfirstsubscriber--onlastunsubscribe)
  - [Scheduled publisher](#scheduled-publisher)
    - [RunPublisher](#runpublisher)
  - [OOB fragments](#oob-fragments)
    - [Component interface](#component-interface)
    - [Raw fragment rendering](#raw-fragment-rendering)
  - [Observability](#observability)
    - [Check for subscribers](#check-for-subscribers)
    - [Topic metrics](#topic-metrics)
    - [Per-topic metrics](#per-topic-metrics)
  - [Testing](#testing)
  - [Topic constants](#topic-constants)
  - [Thread safety](#thread-safety)
  - [Philosophy](#philosophy)
  - [Architecture](#architecture)
    - [SSE flow](#sse-flow)
  - [License](#license)
  <!--toc:end-->

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
messages to subscribers by topic. It is designed to sit behind an HTTP handler
(Echo, Chi, net/http, etc.) and push real-time events to browser clients over
SSE.

## Why

**Without tavern:**

```go
var (
	mu          sync.RWMutex
	subscribers map[string][]chan string
)

func publish(topic, msg string) {
	mu.RLock()
	defer mu.RUnlock()
	for _, ch := range subscribers[topic] {
		select {
		case ch <- msg: // non-blocking send
		default: // drop if full
		}
	}
}

func subscribe(topic string) (chan string, func()) {
	mu.Lock()
	ch := make(chan string, 16)
	subscribers[topic] = append(subscribers[topic], ch)
	mu.Unlock()
	return ch, func() {
		mu.Lock()
		defer mu.Unlock()
		// find and remove ch from the slice, close it...
	}
}

// Now handle graceful shutdown, topic cleanup, subscriber counting...
```

**With tavern:**

```go
broker := tavern.NewSSEBroker()
defer broker.Close()

ch, unsub := broker.Subscribe("events")
defer unsub()

broker.Publish("events", tavern.NewSSEMessage("update", data).String())
// Thread-safe. Non-blocking. Cleanup on Close(). Done.
```

## Install

```bash
go get github.com/catgoose/tavern
```

## Quick start

Create a broker, subscribe, publish, and wire up an SSE endpoint:

```go
broker := tavern.NewSSEBroker()
defer broker.Close()
```

Subscribe to a topic and publish a message:

```go
ch, unsub := broker.Subscribe("events")
defer unsub()

broker.Publish("events", tavern.NewSSEMessage("update", `{"id":1}`).String())

for msg := range ch {
	// handle msg
}
```

Wire it up to an HTTP handler (Echo shown, works with any router):

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
					return nil // broker closed
				}
				if _, err := fmt.Fprint(c.Response(), msg); err != nil {
					return nil // client disconnected
				}
				c.Response().Flush()
			case <-c.Request().Context().Done():
				return nil
			}
		}
	}
}
```

## Core concepts

### Subscribe and unsubscribe

> The server sends a representation. The representation contains links and
> forms. The client follows them. THAT IS THE ENTIRE INTERACTION MODEL.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

The server speaks; the client listens. This is the natural order. `Subscribe` returns a read-only channel and an unsubscribe function. Always
defer the unsubscribe to avoid leaking goroutines and channels.

```go
ch, unsub := broker.Subscribe("events")
defer unsub()

for msg := range ch {
	// handle msg
}
```

### Publish

`Publish` fans out a message to every subscriber of a topic. It is
non-blocking: if a subscriber's channel buffer is full, the message is silently
dropped for that subscriber.

```go
broker.Publish("events", "hello, world")
```

### Scoped subscriptions

Sometimes different clients need to see different slices of the same topic —
per-user notifications, per-tenant feeds, or per-session state. `SubscribeScoped`
tags a subscriber with a scope key. Only messages published via `PublishTo` with
a matching key are delivered to that subscriber.

```go
// Client subscribes to their own user feed
ch, unsub := broker.SubscribeScoped("notifications", userID)
defer unsub()

for msg := range ch {
	// only messages published to this userID arrive here
}
```

Publish to a specific scope:

```go
broker.PublishTo("notifications", userID, tavern.NewSSEMessage("alert", payload).String())
```

For OOB fragments targeted to a single scope:

```go
broker.PublishOOBTo("notifications", userID,
	tavern.Replace("alert-badge", `<span>3</span>`),
)
```

Scoped and unscoped subscribers on the same topic are fully independent.
`Publish` delivers only to unscoped subscribers; `PublishTo` delivers only to
matching scoped subscribers. `HasSubscribers` and `TopicCounts` count both.

### Format SSE messages

`SSEMessage` builds wire-format SSE text. Chain `WithID` and `WithRetry` for
optional fields.

```go
// Minimal message
msg := tavern.NewSSEMessage("update", `{"id":1}`).String()
// event: update
// data: {"id":1}

// With ID and retry
msg := tavern.NewSSEMessage("update", `{"id":1}`).
	WithID("42").
	WithRetry(5000).
	String()
// event: update
// data: {"id":1}
// id: 42
// retry: 5000
```

### Graceful shutdown

`Close` closes all subscriber channels and removes all topics. Pending reads
on subscriber channels will receive the zero value after `Close` returns.

```go
broker.Close()
```

## Publishing variants

> Hypertext is the simultaneous presentation of information and controls such
> that the information BECOMES THE AFFORDANCE through which choices are obtained
> and actions are selected.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

Tavern's publish methods are how the server exercises that affordance in real
time. The state changed; the server tells it.

### PublishWithReplay

Cache recent messages so new subscribers get them immediately on connect:

```go
// Default: replays last 1 message
broker.PublishWithReplay("dashboard", msg)

// Replay last 10 messages (activity feed, chat)
broker.SetReplayPolicy("activity", 10)
broker.PublishWithReplay("activity", msg) // last 10 cached
```

Clear the cache:

```go
broker.ClearReplay("dashboard")
```

### PublishWithID / SubscribeFromID

Track message IDs for gap-free reconnection:

```go
// Publish with an event ID
broker.SetReplayPolicy("events", 100) // keep last 100 for resumption
broker.PublishWithID("events", "evt-42", msg)
broker.PublishWithID("events", "evt-43", msg2)
```

When a client reconnects, the browser sends the `Last-Event-ID` header.
Use `SubscribeFromID` to replay only the missed messages:

```go
func sseHandler(broker *tavern.SSEBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lastID := r.Header.Get("Last-Event-ID")
		ch, unsub := broker.SubscribeFromID("events", lastID)
		defer unsub()
		// ... write messages to response ...
	}
}
```

If `lastID` is empty (first connection), all cached messages are replayed.
If `lastID` is found, only newer messages are replayed. If `lastID` is not
in the replay log (too old), no replay occurs and only live messages are
delivered.

### PublishIfChanged

Skip redundant publishes with content-based deduplication. `PublishIfChanged`
compares the FNV-64a hash of the message against the last published value for
that topic and only publishes when the content actually changed. Returns `true`
if published, `false` if skipped.

```go
// Only publishes when the rendered HTML differs from last time
broker.PublishIfChanged("dashboard", renderDashboard())
```

The dedup state is per-topic and independent of `Publish`. Reset it with
`ClearDedup`:

```go
broker.ClearDedup("dashboard")
```

### PublishDebounced

Debounce waits for a quiet period — if new messages keep arriving, the timer
resets and only the final message is published. Best for user input where you
want the settled state.

```go
// Publish only after 200ms of quiet (typing indicators, search)
broker.PublishDebounced("search-results", html, 200*time.Millisecond)
```

### PublishThrottled

Throttle publishes the first message immediately, then rate-limits to at
most one message per interval. If messages arrive during the cooldown, the most
recent one is published when the interval elapses. Best for continuous data
where you want bounded latency.

```go
// Publish at most once per second, first call immediate
broker.PublishThrottled("live-stats", html, time.Second)
```

## Broker configuration

`NewSSEBroker` accepts functional options to override defaults.

### WithBufferSize

Sets the channel buffer capacity for every new subscriber.
The default is 10. Increase it if your publisher bursts faster than subscribers
drain, or decrease it to tighten back-pressure.

```go
broker := tavern.NewSSEBroker(tavern.WithBufferSize(64))
defer broker.Close()
```

### WithDropOldest

Changes the buffer strategy from drop-newest (default) to
drop-oldest. When a subscriber's buffer is full, the oldest queued message is
discarded to make room for the new one. Use this for dashboards where stale
data is worse than gaps.

```go
broker := tavern.NewSSEBroker(tavern.WithBufferSize(5), tavern.WithDropOldest())
```

### WithKeepalive

Sends `: keepalive\n` comment to all subscribers at the configured interval.
SSE comments are ignored by `EventSource` but keep TCP connections alive
through proxies and load balancers that close idle connections.

```go
broker := tavern.NewSSEBroker(tavern.WithKeepalive(15 * time.Second))
```

### WithTopicTTL

Topics with zero subscribers for longer than the TTL are automatically removed
from the broker's internal maps. Prevents memory leaks from ephemeral
per-resource topics. A background goroutine sweeps at half the TTL interval.

```go
broker := tavern.NewSSEBroker(tavern.WithTopicTTL(5 * time.Minute))
```

### WithLogger

Logs publisher panics and errors when set. Accepts a standard `*slog.Logger`.

```go
broker := tavern.NewSSEBroker(tavern.WithLogger(slog.Default()))
```

### SetReplayPolicy

Controls how many recent messages to cache for replay on a given topic. New
subscribers receive up to `n` cached messages in order before live messages.
Use `n=0` to disable replay for the topic.

```go
// Replay last 10 messages (activity feed, chat)
broker.SetReplayPolicy("activity", 10)
broker.PublishWithReplay("activity", msg) // last 10 cached
```

## Lifecycle hooks

### OnFirstSubscriber / OnLastUnsubscribe

Register callbacks that fire when a topic goes from zero to one subscriber
or from one to zero subscribers. Both scoped and unscoped subscribers are
counted. Callbacks run in their own goroutine and do not block
Subscribe/unsubscribe. Multiple hooks per topic are allowed. Hooks survive
across subscriber cycles.

```go
broker.OnFirstSubscriber("dashboard", func(topic string) {
	go startDashboardPublisher(ctx)
})

broker.OnLastUnsubscribe("dashboard", func(topic string) {
	cancelDashboardPublisher()
})
```

## Scheduled publisher

`ScheduledPublisher` manages multiple named sections with independent
intervals. It ticks on a fast base interval, renders due sections into a
shared buffer, and publishes one batched message per tick. Skips rendering
when no subscribers are listening. Includes panic recovery per section.

```go
pub := broker.NewScheduledPublisher("dashboard", tavern.WithBaseTick(100*time.Millisecond))

pub.Register("network", 1*time.Second, func(ctx context.Context, buf *bytes.Buffer) error {
	return views.NetworkChart(snap).Render(ctx, buf)
})
pub.Register("services", 3*time.Second, func(ctx context.Context, buf *bytes.Buffer) error {
	return views.ServicesPanel(services).Render(ctx, buf)
})

// Blocks until ctx is cancelled. Handles ticking, isDue checks, batching.
broker.RunPublisher(ctx, pub.Start)

// Runtime interval changes
pub.SetInterval("network", 500*time.Millisecond)
```

### RunPublisher

Launches a publisher goroutine with panic recovery. Tracked by the broker's
internal WaitGroup so `Close()` waits for all publishers to return.

```go
broker.RunPublisher(ctx, func(ctx context.Context) {
	// long-running publisher logic
})
```

## OOB fragments

> The whole point -- the ENTIRE POINT -- of hypermedia is that the server tells
> the client what to do next IN THE RESPONSE ITSELF.
>
> -- The Wisdom of the Uniform Interface, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

OOB swaps are SSE's answer to this. The server doesn't just tell the client
that something changed -- it sends the exact DOM mutations to apply. Build
targeted swaps and publish them as a single event:

```go
// Replace, Append, Prepend, Delete — plain string helpers
broker.PublishOOB("events",
	tavern.Replace("stats-bar", "<span>42</span>"),
	tavern.Delete("task-row-5"),
	tavern.Append("activity-feed", "<li>New item</li>"),
)
```

### Component interface

`Component` renders itself to a writer. The interface is identical to
`templ.Component`, so templ components can be passed directly — tavern does
not import templ.

```go
type Component interface {
	Render(ctx context.Context, w io.Writer) error
}
```

Use the component helpers to render and wrap in one call:

```go
broker.PublishOOB("events",
	tavern.ReplaceComponent("stats-bar", views.StatsBar(stats)),
	tavern.AppendComponent("feed", views.FeedItem(item)),
)
```

If rendering fails, the fragment contains an HTML comment with the error
message rather than a partial render.

### Raw fragment rendering

For manual control, render fragments to a string and publish directly:

```go
html := tavern.RenderFragments(
	tavern.Replace("header", "<h1>Updated</h1>"),
	tavern.Delete("old-banner"),
)
broker.Publish("events", tavern.NewSSEMessage("oob", html).String())
```

## Observability

### Check for subscribers

Skip expensive serialization when nobody is listening:

```go
if broker.HasSubscribers("system-stats") {
	stats := collectStats()
	broker.Publish("system-stats", toJSON(stats))
}
```

> Grug say: "before enlightenment: fetch JSON, parse JSON, validate JSON,
> transform JSON, store JSON in client state, derive view from client state,
> diff virtual DOM, reconcile DOM, hydrate DOM, subscribe to store, dispatch
> action, reduce state, re-derive view, re-diff virtual DOM."
>
> Student say: "and after enlightenment?"
>
> Grug say: "`hx-get`"
>
> -- The Recorded Sayings of Layman Grug, [The Dothog Manifesto](https://github.com/catgoose/dothog/blob/main/MANIFESTO.md)

With tavern, after enlightenment: `broker.Publish`.

### Topic metrics

```go
// Per-topic subscriber counts
counts := broker.TopicCounts()
for topic, n := range counts {
	fmt.Printf("%s: %d subscribers\n", topic, n)
}
```

`SubscriberCount` returns the total across all topics in a single call:

```go
fmt.Println("total subscribers:", broker.SubscriberCount())
```

`PublishDrops` reports the running count of messages silently dropped because a
subscriber's buffer was full:

```go
fmt.Println("dropped messages:", broker.PublishDrops())
```

`Stats` combines all three into a single lock acquisition:

```go
s := broker.Stats()
// BrokerStats{Topics: int, Subscribers: int, PublishDrops: int64}
fmt.Printf("topics=%d subs=%d drops=%d\n", s.Topics, s.Subscribers, s.PublishDrops)
```

These are useful for health endpoints, structured logging, and alerting on
unexpected drop rates:

```go
go func() {
	for range time.Tick(30 * time.Second) {
		s := broker.Stats()
		slog.Info("broker", "topics", s.Topics, "subs", s.Subscribers, "drops", s.PublishDrops)
	}
}()
```

### Per-topic metrics

Enable detailed per-topic publish and drop counters with `WithMetrics`:

```go
broker := tavern.NewSSEBroker(tavern.WithMetrics())

// ... publish messages ...

m := broker.Metrics()
for topic, stats := range m.TopicStats {
    fmt.Printf("%s: published=%d dropped=%d peak_subs=%d\n",
        topic, stats.Published, stats.Dropped, stats.PeakSubscribers)
}
fmt.Printf("totals: published=%d dropped=%d\n", m.TotalPublished, m.TotalDropped)
```

Metrics are opt-in — when `WithMetrics` is not set, there is zero overhead on
publish operations. Use this to identify topics with high drop rates, track
publish throughput, and find peak subscriber counts for capacity planning.

## Testing

The `taverntest` subpackage provides helpers for testing code that uses tavern:

```go
import "github.com/catgoose/tavern/taverntest"

func TestMyPublisher(t *testing.T) {
    broker := tavern.NewSSEBroker()
    defer broker.Close()

    rec := taverntest.NewRecorder(broker, "events")
    defer rec.Close()

    // ... code under test publishes to "events" ...
    myPublisher(broker)

    rec.WaitFor(3, time.Second)
    rec.AssertCount(t, 3)
    rec.AssertContains(t, "expected-message")
    rec.AssertNthMessage(t, 0, "first-message")
}
```

`NewScopedRecorder` works the same way for scoped subscriptions:

```go
rec := taverntest.NewScopedRecorder(broker, "notifications", userID)
defer rec.Close()
```

## Topic constants

Tavern ships topic name constants as conventions for common use cases. These
are optional -- any string works as a topic name.

```go
broker.Publish(tavern.TopicActivityFeed, eventJSON)
```

## Thread safety

All `SSEBroker` methods are safe for concurrent use. The broker uses an
`sync.RWMutex` internally: subscribing and unsubscribing take a write lock,
while publishing and reading counts take a read lock. `Publish` snapshots the
subscriber set under the read lock, then sends outside it, so publishers never
block each other.

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

## Architecture

### SSE flow

```
  handler ──► broker.Publish("topic", msg)
                      │
                      ├──► subscriber A (chan) ──► SSE endpoint ──► browser A
                      ├──► subscriber B (chan) ──► SSE endpoint ──► browser B
                      └──► subscriber C (chan) ──► SSE endpoint ──► browser C
```

## License

[MIT](LICENSE)
