# Client-Visible Delivery Observability

Tavern emits control events over the SSE stream to tell clients exactly what
happened during reconnection and subscription changes. These events carry
structured JSON payloads so clients can make informed decisions about data
freshness without guessing.

This document defines the observability contract: which control events fire,
when they fire, what they carry, and what they mean for the client.

---

## Control event reference

| Event | When | Payload | Client meaning |
|---|---|---|---|
| `tavern-reconnected` | Server confirms reconnection complete | `{"replayDelivered": N, "replayDropped": N}` | Recovery is done. Check `replayDropped` for data loss. |
| `tavern-replay-gap` | `Last-Event-ID` not found in replay log | `{"lastEventId": "..."}` | Messages were missed. Region is stale until fresh data arrives. |
| `tavern-replay-truncated` | Replay buffer too small for full recovery | `{"delivered": N, "dropped": N}` | Partial recovery. Some messages lost to buffer limits. |
| `tavern-topics-changed` | Topic added or removed dynamically | `{"action": "added"/"removed", "topic": "...", "topics": [...]}` | Subscription set changed. Full topic list in `topics`. |
| `tavern-backpressure` | Subscriber's backpressure tier changes | `{"tier": "...", "previousTier": "...", "topic": "..."}` | Degradation level changed. Adapt UX to current tier. |

All control events use structured JSON in the SSE `data` field. The event
type (SSE `event` field) is the control event name shown above.

---

## Delivery scenarios

### Clean reconnect (full recovery)

The client reconnects with `Last-Event-ID`. The server finds the ID in the
replay log and replays all missed messages. The client receives:

1. Replayed messages (in order)
2. `tavern-reconnected` with `{"replayDelivered": N, "replayDropped": 0}`

`replayDropped: 0` means every missed message was delivered. The client is
fully caught up.

### Reconnect with truncation (partial recovery)

The client reconnects and the server finds the ID, but the subscriber's
channel buffer is too small to hold all replayed messages. The client
receives:

1. As many replayed messages as fit in the buffer
2. `tavern-replay-truncated` with `{"delivered": N, "dropped": M}`
3. `tavern-reconnected` with `{"replayDelivered": N, "replayDropped": M}`

`replayDropped > 0` means some messages were lost to buffer limits. The
client should treat the region as partially stale and may want to request a
fresh snapshot or reload.

### Reconnect with gap (stale, needs refresh)

The client reconnects but the `Last-Event-ID` is not in the replay log. The
ID rolled out of the ring buffer, the server restarted, or no replay store
was configured. The client receives:

1. `tavern-replay-gap` with `{"lastEventId": "..."}`
2. If a gap fallback policy is configured (`GapFallbackToSnapshot`), the
   snapshot is delivered as the next message
3. `tavern-reconnected` with `{"replayDelivered": 0, "replayDropped": 0}`

The gap event means the client missed an unknown number of messages. If no
snapshot fallback is configured, the client should treat the region as stale
until fresh data arrives from normal publishes.

### Backpressure tier change

When adaptive backpressure is enabled and a subscriber's tier changes (due to
consecutive message drops or recovery after a successful send), the subscriber
receives:

- `tavern-backpressure` with `{"tier": "throttle", "previousTier": "normal", "topic": "dashboard"}`

Tier values: `"normal"`, `"throttle"`, `"simplify"`, `"disconnect"`.

The event fires on both escalation (normal→throttle→simplify→disconnect) and
recovery (any tier→normal after a successful send). For escalation, the
control event is sent via non-blocking send since the channel may be full --
if the subscriber is too backed up to receive even the control event, it is
dropped silently. For disconnect, the event is sent before the channel is
closed.

Clients can use this event to show degradation indicators, suggest the user
refresh, or explain why updates are slower than expected.

### Normal operation (no control events)

During normal connected operation, no control events fire. The client
receives application messages as they are published. The absence of control
events means delivery is healthy.

### Dynamic topic changes

When the server calls `AddTopic` or `RemoveTopic` with `sendControl: true`,
the client receives:

- `tavern-topics-changed` with `{"action": "added", "topic": "new-topic", "topics": ["t1", "t2", "new-topic"]}`

The `topics` array is the complete current subscription set. Clients can use
this to set up or tear down SSE-swap targets dynamically.

---

## How tavern-js consumes these events

The companion library [tavern-js](https://github.com/catgoose/tavern-js)
listens for these control events and translates them into declarative UX
behaviors:

- **`tavern-reconnected`**: Removes reconnecting state classes, restores
  normal appearance. If `replayDropped > 0`, can trigger stale indicators.
- **`tavern-replay-gap`**: Applies stale classes to affected regions, shows
  gap banners prompting the user to refresh.
- **`tavern-replay-truncated`**: Signals partial recovery so the UI can warn
  about incomplete data.
- **`tavern-topics-changed`**: Updates internal subscription tracking.
- **`tavern-backpressure`**: Signals degradation tier changes so the client
  can show backpressure indicators or adapt rendering fidelity.

All of this happens through data attributes on HTML elements -- no custom
JavaScript required for the default behaviors. See the
[tavern-js README](https://github.com/catgoose/tavern-js) for the full
attribute reference.

---

## Extracting payloads client-side

Control events arrive as standard SSE events. In raw `EventSource` usage:

```js
const es = new EventSource("/sse/events");

es.addEventListener("tavern-reconnected", (e) => {
  const { replayDelivered, replayDropped } = JSON.parse(e.data);
  if (replayDropped > 0) {
    console.warn(`Reconnected with ${replayDropped} dropped messages`);
  }
});

es.addEventListener("tavern-replay-gap", (e) => {
  const { lastEventId } = JSON.parse(e.data);
  console.warn(`Gap detected, last known ID: ${lastEventId}`);
});

es.addEventListener("tavern-replay-truncated", (e) => {
  const { delivered, dropped } = JSON.parse(e.data);
  console.warn(`Replay truncated: ${delivered} delivered, ${dropped} dropped`);
});

es.addEventListener("tavern-backpressure", (e) => {
  const { tier, previousTier, topic } = JSON.parse(e.data);
  console.warn(`Backpressure: ${previousTier} → ${tier} on ${topic}`);
});
```

With tavern-js, these listeners are set up automatically and drive the
declarative attribute system.

---

## Subscription termination

Control events travel through the subscriber's queue, so they can be lost
exactly when something goes wrong. To learn why a subscription ended, attach
a `SubscriptionStatus` with `SubWithStatus` (supported by `SubscribeWith`,
`SubscribeFromIDWith` and `SubscribeMultiWith`; `SubscribeGlobWith` panics).

| Reason | Cause |
|---|---|
| `TerminationUnsubscribe` | The caller's unsubscribe function |
| `TerminationDisconnect` | `Disconnect` |
| `TerminationQueueLoss` | A `SubWithDisconnectOnLoss` subscription could not queue a live message, including a `PublishBlocking` timeout |
| `TerminationEvicted` | Slow-subscriber eviction or the adaptive disconnect tier |
| `TerminationBrokerClosed` | `Close`, or subscribing to a closed broker |
| `TerminationReplayFailure` | A failed `ReplayStore` read with `SubWithTerminateOnReplayError`; `Err` is the `*ReplayStoreError` |
| `TerminationRejected` | Admission control refused the subscription |

Guarantees:

- The first cause wins. Later unsubscribe, sibling cancellation or `Close`
  never overwrites it.
- The cause is recorded before the data channel closes, so code that sees the
  channel close can read `Termination()` immediately. `Done()` closes at the
  same moment.
- The status does not depend on the queue. It is readable while the queue is
  full, while the consumer is blocked elsewhere, and after the broker has
  dropped the subscription. It holds one cause and no history.

For a multi-topic subscription the status describes the aggregate. With
`SubWithTerminateOnInnerClose` or `SubWithDisconnectOnLoss` it holds the
cause and `Topic` of the first inner subscription to end, and cancelling the
siblings does not replace it. Without those options the aggregate stays open
until every inner subscription ends, and the status holds the cause of the
last one. The cause is decided when the inner subscriptions end, not when
the fan-in reads their channels. The aggregate's own unsubscribe records
`TerminationUnsubscribe` with an empty topic only if no cause was recorded
before it.

After a loss or replay failure, refetch canonical state before
resubscribing; queued messages that were readable before the close are not
a complete history.

---

## Replay store failures

Every failed `ReplayStore` operation (`Append`, `AfterID`, `Latest`,
`DeleteTopic`, `SetMaxEntries`) is reported as a `*ReplayStoreError` carrying
the operation, the topic and the store's error (`errors.Is` / `errors.As`
work on it). Reports go to `WithReplayStoreErrorHandler`, to the logger when
`WithLogger` is set, and to the `BrokerStats.ReplayStoreErrors` counter. The
handler runs synchronously on the goroutine that called the store, outside
broker locks, so it must return quickly.

- **Writes.** `Append` runs before the live fan-out. A failed `Append` does
  not stop that best-effort delivery attempt, but the message is not retained
  for replay, and the failure says nothing about which subscribers or
  browsers received it. Nothing is retried.
- **Policy calls.** Failed `SetMaxEntries` and `DeleteTopic` calls from
  `SetReplayPolicy` and `ClearReplay` are reported the same way.
- **Reads, default.** The subscription continues and replays whatever
  entries the store returned alongside the error (usually none). A failed
  `AfterID` that reports `found=false` is handled like a Last-Event-ID that
  was not found (gap policy and `tavern-replay-gap`); one that reports
  `found=true` resumes with its entries and `tavern-reconnected`. Clients
  cannot tell this from a genuine gap or a successful resume.
- **Reads, with `SubWithTerminateOnReplayError`.** The subscription ends with
  `TerminationReplayFailure`. No replay entries, gap event, gap snapshot,
  `tavern-reconnected` event or reconnect callback is produced for the failed
  read. A fresh-subscribe snapshot that was already queued stays readable
  before the channel closes.

---

## Queued is not delivered

Tavern's counters and statuses describe its own queues. A message counted as
sent was queued for a subscriber; whether it was written to the connection
depends on the HTTP writer, and whether the browser applied it is known only
to the application. Use client acknowledgements or canonical refetch when
that distinction matters.

---

## Relationship to server-side observability

The control events described here are the **client-visible** half of Tavern's
observability story. On the server side, `OnReconnect` callbacks,
`OnReplayGap` hooks, and the Prometheus/OpenTelemetry export adapters provide
equivalent visibility for operators. The two halves are complementary: server
metrics for dashboards and alerts, client events for UX adaptation.
