# tavern

Thread-safe, topic-based pub/sub broker for Server-Sent Events (SSE) in Go.

Tavern provides a minimal, concurrent-safe message broker that fans out string
messages to subscribers by topic. It is designed to sit behind an HTTP handler
(Echo, Chi, net/http, etc.) and push real-time events to browser clients over
SSE.

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

`Subscribe` returns a read-only channel and an unsubscribe function. Always
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

## Topic constants

Tavern ships a set of topic name constants as conventions for common use
cases (dashboards, activity feeds, etc.). These are optional -- any string
works as a topic name.

```go
broker.Publish(tavern.TopicSystemStats, statsJSON)
broker.Publish(tavern.TopicActivityFeed, eventJSON)
```

## Thread safety

All `SSEBroker` methods are safe for concurrent use. The broker uses an
`sync.RWMutex` internally: subscribing and unsubscribing take a write lock,
while publishing and reading counts take a read lock. `Publish` snapshots the
subscriber set under the read lock, then sends outside it, so publishers never
block each other.

## License

[MIT](LICENSE)
