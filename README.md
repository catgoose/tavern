# tavern

[![Go Reference](https://pkg.go.dev/badge/github.com/catgoose/tavern.svg)](https://pkg.go.dev/github.com/catgoose/tavern)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

![tavern](https://raw.githubusercontent.com/catgoose/screenshots/main/tavern/tavern.png)

Thread-safe, topic-based pub/sub broker for Server-Sent Events (SSE) in Go.

> Master say: "but how does the client know when state changes?"
>
> Grug say: "server tell it."
>
> -- Layman Grug

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
        default:        // drop if full
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

## Usage

### Create a broker

```go
broker := tavern.NewSSEBroker()
defer broker.Close()
```

### Subscribe and unsubscribe

> The server sends a representation. The representation contains links and forms. The client follows them. THAT IS THE ENTIRE INTERACTION MODEL.
>
> -- The Wisdom of the Uniform Interface

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

### Check for subscribers

Skip expensive serialization when nobody is listening:

```go
if broker.HasSubscribers("system-stats") {
    stats := collectStats()
    broker.Publish("system-stats", toJSON(stats))
}
```

> Before enlightenment: fetch JSON, parse JSON, validate JSON, transform JSON, store JSON in client state, derive view from client state, diff virtual DOM, reconcile DOM. After enlightenment: the server tells it.
>
> -- Layman Grug (adapted for SSE)

### Echo SSE endpoint

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

### Topic metrics

```go
// Per-topic subscriber counts
counts := broker.TopicCounts()
for topic, n := range counts {
    fmt.Printf("%s: %d subscribers\n", topic, n)
}
```

## OOB Fragments

Tavern includes helpers for HTMX out-of-band (OOB) swaps over SSE. Build
targeted DOM mutations and publish them as a single event:

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

## Topic constants

Tavern ships a set of topic name constants as conventions for common use
cases (dashboards, activity feeds, etc.). These are optional -- any string
works as a topic name.

```go
broker.Publish(tavern.TopicSystemStats, statsJSON)
broker.Publish(tavern.TopicActivityFeed, eventJSON)
```

> The whole point -- the ENTIRE POINT -- of hypermedia is that the server tells the client what to do next IN THE RESPONSE ITSELF.
>
> -- The Wisdom of the Uniform Interface

SSE is the server telling the client what happened next, in real time. The event stream is just another representation — the server speaks, the client listens, and nobody had to install an npm package to make it work.

## Thread safety

All `SSEBroker` methods are safe for concurrent use. The broker uses an
`sync.RWMutex` internally: subscribing and unsubscribing take a write lock,
while publishing and reading counts take a read lock. `Publish` snapshots the
subscriber set under the read lock, then sends outside it, so publishers never
block each other.

## Philosophy

Tavern follows the [dothog design philosophy](https://github.com/catgoose/dothog/blob/main/PHILOSOPHY.md): the server drives state, the broker is just plumbing, and `sync.RWMutex` is the only dependency you need for thread safety.

> wife of Grug say from cave: "easy, easy, easy. like touching feet to ground when get out of bed. server return html. browser render html. what is difficult?"
>
> -- Layman Grug

Server publish event. Browser receive event. What is difficult?

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
