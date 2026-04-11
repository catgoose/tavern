# Lifeline + Scoped Stream Contract

This document codifies the contract between lifeline and scoped streams in
a Tavern app-shell architecture. It defines what each stream is responsible
for, when to use each pattern, and what guarantees the system provides.

For implementation examples, see
[Recipe 27: App-shell lifeline with scoped panel streams](../RECIPES.md#recipe-27-app-shell-lifeline-with-scoped-panel-streams)
and the [App-shell lifeline architecture](../README.md#app-shell-lifeline-architecture)
section of the README.

These patterns build on the topic vocabulary defined in
[Topic Semantics](topic-semantics.md) and the multiplexing guidance in
[Page-Level Multiplexing](page-multiplexing.md).

---

## Stream roles

### Lifeline stream

A **lifeline stream** is a long-lived, app-wide SSE connection that stays
open for the entire user session. It carries control-plane and
low-bandwidth topics.

- Created with `SubscribeMultiWithMeta` so the subscriber has an ID that
  `AddTopic` / `RemoveTopic` can target.
- Topics are mutated dynamically as the user navigates. The connection
  itself is never torn down for navigation.
- Carries: notifications, presence, nav-state, invalidation signals,
  small counters, theme broadcasts.

### Scoped stream

A **scoped stream** is a short-lived, high-bandwidth SSE connection tied
to a specific view, panel, or widget. It has its own buffer, backpressure,
and eviction policy.

- Created with `SubscribeMulti`, `SubscribeScoped`, or a dedicated
  `SSEHandler` endpoint.
- Torn down when the view is dismissed (e.g., user navigates away, panel
  closes, widget is removed from the DOM).
- Carries: charts, feeds, detail-panel data, high-frequency
  section-specific updates.

---

## Contract points

### 1. What belongs on each stream

| Stream | Content | Rate | Examples |
|--------|---------|------|----------|
| **Lifeline** | Control-plane, low-bandwidth | < 1 msg/s typical | notifications, presence, nav-state, invalidation signals |
| **Scoped** | Data-plane, high-bandwidth | Unbounded (with backpressure) | chart updates, activity feeds, log tails, real-time metrics |

**Rule of thumb:** if a topic produces more than a few messages per second
under load, it belongs on a scoped stream. If it is relevant across the
entire session regardless of what view is active, it belongs on the
lifeline.

### 2. When to use AddTopic/RemoveTopic vs a separate scoped connection

Use `AddTopic` / `RemoveTopic` on the lifeline when:

- The new topic is low-bandwidth and shares the lifeline's lifecycle
  characteristics.
- You want a single reconnection path for all active topics.
- The topic set changes with navigation but the data rate stays low.

Open a separate scoped connection when:

- The topic is high-bandwidth and could cause backpressure on the
  lifeline's low-frequency topics.
- The view needs independent buffer sizing, eviction, or
  `MaxConnectionDuration`.
- The view needs per-user scoping via `SubscribeScoped` / `PublishTo`.
- You want the stream's lifecycle to match the DOM element's lifecycle
  exactly (element removed = connection closed).

### 3. tavern-topics-changed guarantees

When `AddTopic` or `RemoveTopic` is called with `sendControl: true`, the
subscriber receives a `tavern-topics-changed` SSE event with a JSON
payload:

```json
{
  "action": "added",
  "topic": "dashboard-data",
  "topics": ["notifications", "nav-state", "dashboard-data"]
}
```

**Guarantees:**

- `action` is `"added"` or `"removed"` -- the delta that just occurred.
- `topic` is the single topic that was added or removed.
- `topics` is the **full current topic list** after the change. The client
  can use this to reconstruct state from scratch (useful after reconnect
  or if delta processing fails).
- The event is delivered on the same SSE connection, in-order with other
  events. No events for the new topic will arrive before the
  `tavern-topics-changed` event for its addition.

### 4. Failure isolation

Lifeline and scoped streams are fully independent connections. A failure
in one does not affect the other:

- **Scoped stream errors** do not propagate to the lifeline. The lifeline
  continues delivering control events uninterrupted.
- **Lifeline errors** do not tear down scoped streams. Each stream
  reconnects independently.
- The client (tavern-js) can dispatch `tavern:stream-fallback` when a
  scoped stream enters an error state, allowing the app shell to show
  fallback UI for that panel while the lifeline remains healthy.

### 5. Reconnection behavior

Each stream reconnects independently using the browser's native
`EventSource` reconnection with `Last-Event-ID`:

- **Lifeline replay** covers only the lifeline's topics. It uses
  `SubscribeFromID` to resume from where it left off.
- **Scoped stream replay** covers only that stream's topics. Its buffer
  and gap strategy are configured independently.
- **No cross-contamination.** A lifeline reconnect never replays scoped
  stream events and vice versa. Each stream has its own replay buffer and
  its own `Last-Event-ID` sequence.
- Gap strategies (`GapSilent`, `GapFallbackToSnapshot`) apply per-topic
  within each stream. A gap in one topic does not affect other topics on
  the same connection.

### 6. Anti-patterns

| Anti-pattern | Problem | Fix |
|---|---|---|
| **Firehose** -- everything on one stream | High-bandwidth topics cause backpressure that stalls low-frequency control events. Buffer overflows drop notifications. | Split into lifeline (control) + scoped (data) streams. |
| **Reconnect-as-navigation** -- tear down and re-establish the SSE connection on every route change | Unnecessary latency, missed events during the reconnect window, no replay continuity. | Use `AddTopic` / `RemoveTopic` on the lifeline, or open/close scoped streams per view. |
| **Duplicate DOM ownership** -- two streams updating the same element | Race conditions, flicker, unpredictable state. One stream's update overwrites the other's. | Each DOM region is owned by exactly one stream. Use distinct `sse-swap` targets. |
| **Scoped stream for everything** -- opening a new connection for every low-frequency topic | Connection overhead, unnecessary reconnection complexity, wasted server resources. | Low-frequency topics belong on the lifeline with `AddTopic`. |
| **Lifeline as data pipe** -- routing high-bandwidth data through the lifeline | Backpressure from data topics delays control events. | High-bandwidth data belongs on scoped streams. |

---

## Decision guidance

| Scenario | Use lifeline AddTopic | Use scoped stream |
|---|---|---|
| Low-frequency status updates | Yes | No |
| High-bandwidth data feed | No | Yes |
| Ephemeral panel data (low rate) | Yes | Either |
| Ephemeral panel data (high rate) | No | Yes |
| App-wide notifications | Yes | No |
| User navigates to a new view (control data) | AddTopic on lifeline | -- |
| User navigates to a new view (heavy data) | -- | New scoped connection |
| Per-user isolated panel data | No | Yes (`SubscribeScoped` / `PublishTo`) |
| Independent buffer/eviction needed | No | Yes |

**When in doubt:** start with `AddTopic` on the lifeline. If you observe
backpressure affecting control topics, or you need per-view buffer tuning,
move that topic to a scoped stream.

---

## Relationship to other patterns

- **Page-level multiplexing** ([docs/page-multiplexing.md](page-multiplexing.md)):
  covers multiplexing multiple topics on a single connection for a
  server-rendered page. The lifeline is a special case of multiplexing
  that spans the entire session rather than a single page.
- **Snapshot and replay** ([docs/snapshot-replay.md](snapshot-replay.md)):
  covers per-topic replay strategies. Both lifeline and scoped streams
  use these independently.
- **Topic semantics** ([docs/topic-semantics.md](topic-semantics.md)):
  defines topic naming conventions. Lifeline topics tend to be
  `notify/*`, `presence/*`, `nav/*` categories. Scoped stream topics
  tend to be `resource/*`, `collection/*`, or domain-specific.
