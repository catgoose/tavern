# Production Operations

A starting point for running Tavern-backed SSE in production: which settings
interact, what the signals mean, and how clients are expected to recover. It
describes Tavern's current delivery semantics. Proxy and platform settings are
suggestions to verify in your environment, not tested recipes; the sections
say which behavior is covered by Tavern's own tests.

Related references:

- [delivery-observability.md](delivery-observability.md): control events,
  termination reasons and replay store failures
- [snapshot-replay.md](snapshot-replay.md): choosing snapshot, replay or both
- [stream-contract.md](stream-contract.md): lifeline and scoped stream roles

---

## Starting configuration

```go
broker := tavern.NewSSEBroker(
    tavern.WithBufferSize(32),               // queued messages per subscriber
    tavern.WithKeepalive(20*time.Second),    // below every idle timeout on the path
    tavern.WithSlowSubscriberEviction(64),   // consecutive drops before eviction
    tavern.WithMaxSubscribers(20_000),       // shed load instead of exhausting memory
    tavern.WithLogger(logger),
    tavern.WithReplayStoreErrorHandler(func(e *tavern.ReplayStoreError) {
        replayStoreErrors.WithLabelValues(e.Op.String()).Inc()
    }),
)
broker.SetReplayPolicy("orders", 50)
broker.SetReplayGapPolicy("orders", tavern.GapFallbackToSnapshot, renderOrders)

mux.Handle("/sse/orders", broker.SSEHandler("orders",
    tavern.WithWriteTimeout(10*time.Second),
    tavern.WithMaxConnectionDuration(30*time.Minute),
    tavern.WithReconnectDelay(3*time.Second),
    tavern.WithWriteErrorHandler(func(e *tavern.WriteError) {
        sseWriteErrors.WithLabelValues(e.Op.String()).Inc()
    }),
))

srv := &http.Server{
    Handler:           mux,
    ReadHeaderTimeout: 10 * time.Second,
    // WriteTimeout stays 0: it would cut every stream at a fixed age.
    // WithWriteTimeout bounds each write instead.
}
```

The numbers are placeholders. Size them from your own traffic (see
[Capacity inputs](#capacity-inputs)).

---

## The request path

### Proxies and load balancers

SSE needs every hop to pass bytes through as they are written and to keep an
idle connection open.

- **Disable response buffering** for SSE routes. A buffering proxy holds
  frames until its buffer fills, so clients see bursts or nothing. With nginx,
  `proxy_buffering off;` on the location (or an `X-Accel-Buffering: no`
  response header) does this.
- **Disable compression** for `text/event-stream` unless the proxy is known
  to flush per frame; compressors buffer too.
- **Raise idle/read timeouts** above the keepalive interval. A proxy that
  sees no bytes for its idle timeout closes the stream; `WithKeepalive`
  sends comment frames to prevent that.
- **HTTP/1.1 connection limits.** Browsers allow about six HTTP/1.1
  connections per origin, shared by all tabs. Serve SSE over HTTP/2, or
  multiplex topics onto one stream with `GroupHandler`/`DynamicGroupHandler`
  or `SubscribeMulti`.

### Go server settings

- `http.Server.WriteTimeout` is measured from the start of the response, so
  it ends every stream at that age. Leave it at 0 for SSE routes and use
  `WithWriteTimeout`.
- `ReadHeaderTimeout` is safe and recommended.
- `http.Server.Shutdown` does not end active streams on its own: it waits
  for connections to go idle, which SSE connections never do. See
  [Graceful shutdown](#graceful-shutdown).

### Writer capabilities and middleware

Tavern's handlers and `StreamSSE` need to flush, and `WithWriteTimeout` /
`WithStreamWriteTimeout` need write deadlines. Both are reached through
[`http.ResponseController`](https://pkg.go.dev/net/http#ResponseController),
so middleware that wraps the `ResponseWriter` must implement
`Unwrap() http.ResponseWriter` (or `Flush`/`SetWriteDeadline` itself).

- Without flushing, `StreamSSE` returns an error and the handlers still
  stream, but frames sit in the server's buffer.
- Without deadline support, a handler configured with `WithWriteTimeout`
  responds 500 before subscribing and reports a `WriteError` matching
  `http.ErrNotSupported`; `StreamSSE` returns that error. Tavern does not
  stream without a bound that was asked for.
- A custom `WithSSEWriter` runs under the deadline, but only writes to the
  connection are interrupted by it. A writer that blocks elsewhere is not.
- Tavern's tests cover deadlines over net/http HTTP/1.1 connections
  (including a client that stops reading) and over writers that expose
  `SetWriteDeadline` directly or through `Unwrap`. HTTP/2 deadline behavior
  comes from net/http and is not separately tested here.

---

## Timers and how they relate

| Setting | What it does | Relationship |
|---|---|---|
| `WithKeepalive(d)` | Comment frame to every subscriber every `d` | Shorter than the smallest idle timeout on the path |
| `WithWriteTimeout(d)` | Bounds each frame write and flush; cleared between frames | Long enough for one frame on a slow but healthy client; it is not an idle limit |
| `WithMaxConnectionDuration(d)` | Ends a stream after `d` plus up to 10% jitter, with a `retry:` hint | Rotates connections across deploys and load balancers; jitter spreads reconnects |
| `WithReconnectDelay(d)` | The `retry:` value sent at max duration | Browser waits about this long before reconnecting |
| `SetRetry` / `SetRetryAll` | Queues a `retry:` directive for live subscribers | Use before shutdown so clients back off |
| Request context | Ends the stream; with `WithWriteTimeout` it also expires a pending write deadline | Only interrupts writes on deadline-capable writers |

A write that times out, a failed flush and a failed write all end the
connection. The browser's `EventSource` reconnects and resumes with
`Last-Event-ID`.

---

## Replay retention and reconnect gaps

- `SetReplayPolicy(topic, n)` keeps the last `n` messages, by count, not by
  time. Whether a reconnect finds its `Last-Event-ID` depends on how many
  messages were published while the client was away. Size `n` from publish
  rate × the outage you want to cover.
- Replay is queued into the subscriber buffer, so `WithBufferSize` caps how
  much one reconnect can deliver. On a resume, overflow is reported as
  `tavern-replay-truncated`.
- A missing ID is a gap. With `GapFallbackToSnapshot` the client gets
  `tavern-replay-gap` and a fresh snapshot; with the default `GapSilent` it
  only resumes live. Design clients to **refetch canonical state** on a gap,
  truncation or termination rather than trusting partial history.
- With a `ReplayStore`, store failures are reported (see
  [delivery-observability.md](delivery-observability.md#replay-store-failures)).
  By default a failed read can look like a gap or a successful resume; use
  `SubWithTerminateOnReplayError` where that matters.

### Multiple topics on one stream

An `EventSource` remembers one `Last-Event-ID` per connection: the last ID it
received on any topic. `GroupHandler` resumes every topic from that single
ID, so topics whose replay log does not contain it are treated as gaps. If
each topic needs its own cursor, carry topic-scoped cursors in your own
protocol (for example a query parameter on reconnect) or give the topics
separate streams.

---

## Choosing a delivery policy

Delivery is best-effort by default: a full subscriber queue drops the new
message, `Publish` never blocks, and the drop is counted.

| Data | Policy | Client recovery |
|---|---|---|
| Latest value matters, history does not (tickers, gauges) | `SubscribeWithCoalescing` or `SubWithRate` | None needed; the next value replaces the old one |
| A lost message means the view is wrong (invalidations) | `SubWithDisconnectOnLoss` | Stream ends; refetch canonical state, then resubscribe |
| Small number of important events | `SetReplayPolicy` + `PublishWithID` | Resume with `Last-Event-ID`; refetch on gap |
| Slow clients should not hold memory | `WithSlowSubscriberEviction` or `WithAdaptiveBackpressure` | Reconnect; refetch if history matters |
| Producer must not outrun consumers | `PublishBlocking` with a timeout | The producer handles `ErrPublishTimeout` |

`WithDropOldest` keeps the newest messages instead, which suits feeds but not
invalidation streams (loss-aware subscriptions ignore it).

---

## Reconnect bursts

A deploy, proxy restart or `Close` disconnects every client at once.

- Send `SetRetryAll` with a randomized or generous delay before shutting
  down.
- `WithMaxConnectionDuration` adds jitter so steady-state rotation does not
  synchronize clients.
- `WithMaxSubscribers` / `WithMaxSubscribersPerTopic` / `WithAdmissionControl`
  refuse subscribers instead of overcommitting; `SSEHandler` answers 503 and
  `EventSource` retries on its own schedule.
- Gap snapshots run once per reconnecting subscriber. Keep snapshot functions
  cheap or cached, because a burst calls them many times at once.

---

## Capacity inputs

Tavern bounds queues by message count, not bytes. Estimate the worst case:

- Live queues: subscribers × `WithBufferSize` × largest message size.
- Replay: topics × replay policy `n` × message size (in-memory store).
- Plus one goroutine per streaming connection and per multi-topic fan-in.

There are no general capacity numbers here; measure with your message sizes,
fan-out and client behavior. Keep large payloads out of the stream where you
can (send an invalidation and let the client fetch), which bounds both queue
memory and slow-client impact.

---

## Interpreting signals

| Signal | Where | Meaning |
|---|---|---|
| `PublishDrops`, per-topic `Dropped` | `Stats()`, `Metrics()`, adapters | Messages not queued because a buffer was full |
| Evictions | adapters, `WithSlowSubscriberCallback` | Slow subscribers removed |
| Termination reason | `SubWithStatus` | Why one subscription ended: `queue-loss`, `evicted`, `disconnect`, `broker-closed`, `replay-failure`, ... |
| `ReplayStoreErrors` | `Stats()`, `WithReplayStoreErrorHandler` | Store operations that failed; live delivery continued |
| `WriteError` by op | `WithWriteErrorHandler`, `WithStreamWriteErrors` | `write`/`flush` failures are usually disconnects; a timeout (`os.ErrDeadlineExceeded`) is a slow client; `deadline` with `http.ErrNotSupported` is a configuration problem |
| Subscriber lag, connection duration | `WithObservability` | Queue depth and how long streams last |

Every counter describes Tavern's queues and writes. A queued or written
message was not necessarily received or applied by the browser; use
application-level acknowledgements where that matters.

The `tavernprom` and `tavernotel` adapters export published and dropped
counts, subscriber and topic gauges, publish latency, throughput, evictions
and connection durations. They do not export replay store errors, write
errors or termination reasons; record those from the handlers above.

---

## Graceful shutdown

```go
// 1. Tell connected browsers to wait before reconnecting.
broker.SetRetryAll(5 * time.Second)

// 2. Stop accepting new connections and wait for handlers to return.
//    Closing the broker ends every stream: queued messages (including the
//    retry directive) are written first if the client keeps up.
srv.RegisterOnShutdown(broker.Close)
ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()
_ = srv.Shutdown(ctx)
```

`broker.Close` waits for publishers started with `RunPublisher`, then closes
every subscriber channel; handlers write what is already queued and return.
A client that is not reading can hold its handler until `WithWriteTimeout`
expires, which is another reason to set it.

---

## Authorization

Tavern does not authenticate requests or check topic access.

- Authenticate and authorize **before** the handler subscribes, in middleware
  or in the `DynamicGroup` function that picks topics per request.
- Scopes (`SubscribeScoped`, `PublishTo`, `SubWithScope`) isolate delivery,
  not access: derive the scope from the server-side identity, never from a
  client-supplied value.
- Topic names are not secrets. Anything a client can name, it can request if
  the route allows it.

---

## Troubleshooting

| Symptom | Likely cause | Tavern control | Expected client recovery |
|---|---|---|---|
| Events arrive in bursts or only at disconnect | Proxy or compression buffering | Disable buffering/compression for the route | None once fixed |
| Streams drop every N seconds with no errors | Idle timeout on a proxy or load balancer | `WithKeepalive` below that timeout | `EventSource` reconnects with `Last-Event-ID` |
| Streams end at a fixed age | `http.Server.WriteTimeout` | Set it to 0; use `WithWriteTimeout` | Reconnect |
| 500 on SSE routes after adding `WithWriteTimeout` | Middleware hides `SetWriteDeadline` | Add `Unwrap` to the wrapper | n/a |
| Rising drops on one topic | Slow consumers or bursts larger than the buffer | `WithBufferSize`, coalescing, eviction or loss-aware subscriptions | Depends on policy (see above) |
| `tavern-replay-gap` after short outages | Replay policy too small for the publish rate | Raise `SetReplayPolicy`; add a gap snapshot | Refetch canonical state |
| Handler goroutines pile up on bad networks | Writes blocked on clients that stopped reading | `WithWriteTimeout` | Reconnect |
| Thundering herd after deploys | All clients reconnect at once | `SetRetryAll`, max-duration jitter, admission limits | Reconnect after the retry delay |
| `ReplayStoreErrors` increasing | Store unavailable | Fix the store; `SubWithTerminateOnReplayError` for strict resumes | Refetch canonical state |
