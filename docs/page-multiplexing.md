# Page-Level Multiplexing for Single-Connection Live Pages

A server-rendered page often has multiple live regions: a data table, a
notification bar, a presence list, an activity feed. Opening a separate
EventSource for each is wasteful. Tavern supports carrying all of these
over a single SSE connection. This document explains when to multiplex
and how.

These patterns build on the topic vocabulary defined in
[Topic Semantics](topic-semantics.md) and the replay/snapshot strategies
defined in [Snapshot and Replay Patterns](snapshot-replay.md).

---

## When to multiplex vs separate connections

**Multiplex** when:

- All topics share the same lifecycle (page load to page unload).
- The page is server-rendered and the regions are OOB swap targets.
- You want one reconnection path instead of N.

**Separate connections** when:

- Regions have independent lifecycles -- a widget that outlives the page.
- Topics require different auth contexts.
- One region is high-frequency and others are low-frequency. Backpressure
  affects the entire connection: a slow consumer on one topic stalls
  delivery for all topics on that connection.

**Rule of thumb:** if the topics load and unload together, they belong on
one connection.

---

## Choosing the right subscription type

### StaticGroup (`DefineGroup` / `GroupHandler`)

Every visitor to this page sees the same set of topics. Define the group
once at startup; wire the handler.

```go
broker.DefineGroup("ops-dashboard", []string{
    "admin/health", "admin/deploys", "admin/circuit-breakers",
})
mux.Handle("/sse/ops", broker.GroupHandler("ops-dashboard"))
```

Use when: the topic set is known at compile time, authorization is
handled elsewhere (middleware), and all connections get the same stream.

### DynamicGroup (`DynamicGroup` / `DynamicGroupHandler`)

Topics depend on who is viewing. The group resolver reads the request
and returns the topic set.

```go
broker.DynamicGroup("project-page", func(r *http.Request) []string {
    id := chi.URLParam(r, "id")
    return []string{
        "resource/projects/" + id,
        "collection/projects/" + id + "/tasks",
        "presence/projects/" + id,
    }
})
mux.Handle("/sse/projects/{id}", broker.DynamicGroupHandler("project-page"))
```

Use when: the topic set varies by route params, auth context, or tenant.
This is the most common choice for detail pages.

### SubscribeMulti

Assemble the topic set programmatically at the handler level. More
flexible than groups but requires manual SSE writing.

```go
ch, unsub := broker.SubscribeMulti(
    "collection/projects",
    "notify/user/" + userID,
)
```

Use when: the topic set varies by feature flags, complex authorization,
or other logic that does not fit a single resolver function.

---

## Page composition patterns

### Project detail page

Three topic categories on one connection via DynamicGroup:

| Topic | Category | Replay strategy |
|-------|----------|-----------------|
| `resource/projects/7` | Resource | Small replay buffer + snapshot fallback |
| `collection/projects/7/tasks` | Collection | Larger replay buffer + full snapshot on gap |
| `presence/projects/7` | Presence | Snapshot only, no replay |

Each topic publishes OOB fragments targeting its own DOM region. The
browser processes them in arrival order.

```go
// Project metadata changed
broker.PublishOOB("resource/projects/7",
    tavern.Replace("project-header", renderProjectHeader(project)))

// Task added to the collection
broker.PublishOOB("collection/projects/7/tasks",
    tavern.Append("task-list", renderTaskRow(task)))

// Presence updated
broker.PublishOOB("presence/projects/7",
    tavern.Replace("presence-avatars", renderPresence(participants)))
```

### Ops dashboard

All admin topics via StaticGroup. Low-volume, high-importance, generous
replay buffers.

```go
broker.DefineGroup("ops", []string{
    "admin/health", "admin/deploys", "admin/circuit-breakers",
})
broker.SetReplayPolicy("admin/health", 100)
broker.SetReplayPolicy("admin/deploys", 100)
broker.SetReplayPolicy("admin/circuit-breakers", 100)
```

### User home page

Mixed categories scoped by auth context:

```go
broker.DynamicGroup("user-home", func(r *http.Request) []string {
    userID := auth.UserID(r.Context())
    return []string{
        "notify/user/" + userID,
        "collection/projects",
    }
})
```

Notifications replay with TTL (stale alerts auto-expire). The project
collection replays deltas with snapshot fallback on gap.

---

## OOB fragments for multi-region updates

Each topic publishes OOB fragments targeting its DOM region. Use distinct
`hx-swap-oob` target IDs per region to avoid conflicts.

```html
<div hx-ext="sse" sse-connect="/sse/projects/7">
  <header id="project-header" sse-swap="resource/projects/7"></header>
  <ul id="task-list" sse-swap="collection/projects/7/tasks"></ul>
  <div id="presence-avatars" sse-swap="presence/projects/7"></div>
</div>
```

Multiple topics can update simultaneously. GroupHandler uses the topic
name as the SSE event type, so `sse-swap` attributes match topic names
directly. Each fragment targets a unique DOM ID; there is no collision.

For atomic multi-fragment updates within a single topic, use
`RenderFragments` to batch them:

```go
broker.PublishOOB("resource/projects/7",
    tavern.Replace("project-header", renderHeader(p)),
    tavern.Replace("project-status", renderStatus(p)),
)
```

Both fragments arrive in one SSE message and swap atomically.

---

## Dynamic topic changes

`AddTopic` and `RemoveTopic` modify a subscriber's topic set without
tearing down the connection. This requires `SubscribeMultiWithMeta` so
the subscriber has an ID that AddTopic/RemoveTopic can target.

**Use case:** A user navigates between tabs within a page. The server
adds topics for the new tab and removes the old ones.

```go
// User opens the analytics tab
broker.AddTopic(sessionID, "collection/projects/7/analytics", true)
broker.RemoveTopic(sessionID, "collection/projects/7/tasks", true)
```

The `sendControl: true` parameter emits a `tavern-topics-changed` event
so the client can set up new swap targets or tear down old ones. The
connection stays alive; only the content flowing through it changes.

Topic changes are server-initiated. The server decides what the client
sees. If the client needs to request a change, it sends a normal HTTP
request (e.g., `POST /navigate`) and the server calls AddTopic/RemoveTopic
in response.

---

## Reconnection with multiplexed subscriptions

On reconnect, each topic in the group handles replay and snapshot
independently per its category:

- Resource topics replay from the small buffer or fall back to snapshot.
- Collection topics replay deltas or fall back to a full collection snapshot.
- Presence topics always re-snapshot. Stale join/leave events are never
  replayed.
- Notification topics replay only unexpired entries (TTL filtering).

The client experiences one reconnection event, not one per topic.
GroupHandler sends a single `Last-Event-ID` and replays each topic's
buffer in sequence before resuming the live stream.

`SetBundleOnReconnect` applies per-topic. Bundle collection deltas to
reduce DOM churn; do not bundle presence snapshots (they are already a
single message).

```go
broker.SetBundleOnReconnect("collection/projects/7/tasks", true)
// Presence snapshot is one message -- bundling adds nothing.
```

Gap handling works per-topic. A gap in one topic triggers that topic's
fallback (snapshot) without affecting other topics on the same connection.

---

## Anti-patterns

**One mega-topic per page with client-side routing.** Publishing all page
data to a single topic and letting the client sort it out violates
server-owns-representation. Use separate topics; let Tavern multiplex them.

**Opening N EventSource connections when topics share a lifecycle.** If
all topics load and unload with the page, use a group or SubscribeMulti
instead of N independent connections.

**Client-initiated topic negotiation over the SSE connection.** SSE is
server-to-client. The server decides what flows; use DynamicGroup to
resolve topics from request context, or AddTopic/RemoveTopic from a
normal HTTP handler.

**Mixing high-frequency and low-frequency topics without delivery
shaping.** A 100 msg/s sensor feed on the same connection as a
once-per-minute notification stream means backpressure from the sensor
feed can stall notifications. Either use separate connections or apply
per-topic delivery shaping (debounce, throttle) to bring rates into the
same order of magnitude.
