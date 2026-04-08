# Snapshot and Replay Patterns for Live Documents

Tavern delivers server-owned representations honestly. Replay and gap
handling exist so the server can be honest about what the client missed. A
delivery layer that silently drops messages is lying. A delivery layer that
admits gaps and falls back to snapshots is honest.

This document defines when and how to use snapshots vs replay for each topic
category defined in [Topic Semantics](topic-semantics.md). These are not
enforced by the library. They are patterns that keep reconnection honest
across every topic shape.

---

## Replay vs snapshot -- when to use which

### Resource topics (`resource/<type>/<id>`)

Snapshot-first is natural. A reconnecting client wants "what does this look
like now?", not a history of intermediate states. Use
`SubscribeWithSnapshot` to deliver the current representation on connect.

Replay from `Last-Event-ID` is useful for short disconnects where the delta
is small, but the fallback should always be a fresh snapshot. Configure this
with `SetReplayGapPolicy(topic, GapFallbackToSnapshot, renderCurrent)`.

```go
broker.SetReplayPolicy("resource/tasks/42", 5)
broker.SetReplayGapPolicy("resource/tasks/42", tavern.GapFallbackToSnapshot, func() string {
    return renderTask(42)
})
ch, unsub := broker.SubscribeWithSnapshot("resource/tasks/42", func() string {
    return renderTask(42)
})
```

A small replay buffer (5-10 entries) covers brief disconnects. Everything
else gets a snapshot. The current state of a resource is always a complete
answer.

### Collection topics (`collection/<type>`)

Snapshot on connect delivers the full set. Live updates deliver deltas --
additions, removals, reordering. Replay from `Last-Event-ID` works well
because deltas are ordered. On gap, fall back to a full collection
snapshot. Partial deltas with unknown gaps corrupt the view.

```go
broker.SetReplayPolicy("collection/tasks", 50)
broker.SetReplayGapPolicy("collection/tasks", tavern.GapFallbackToSnapshot, func() string {
    return renderTaskList()
})
ch, unsub := broker.SubscribeWithSnapshot("collection/tasks", func() string {
    return renderTaskList()
})
```

A larger replay buffer (50+) makes sense here -- collection deltas are
small and replaying adds/removes is cheaper than re-rendering the set.

### Presence topics (`presence/<scope>`)

Snapshot on connect delivers who is here now. Live updates deliver join,
leave, and update events. Replay is low-value for presence because stale
join/leave events are misleading. On reconnect, always re-snapshot. Do not
replay old join/leave events.

```go
// No replay policy -- presence does not benefit from replay.
ch, unsub := broker.SubscribeWithSnapshot("presence/room/general", func() string {
    return renderParticipants("room/general")
})
```

Skip `SetReplayPolicy` entirely for presence topics. The snapshot is the
only honest answer to "who is here?".

### Admin and ops topics (`admin/<concern>`)

Replay from `Last-Event-ID` is usually fine. These are low-volume and
ordered. Gaps are unlikely but if they occur, a snapshot of current system
state is the right fallback.

```go
broker.SetReplayPolicy("admin/health", 100)
broker.SetReplayGapPolicy("admin/health", tavern.GapFallbackToSnapshot, func() string {
    return renderSystemHealth()
})
```

A generous replay buffer is cheap for low-volume topics and covers long
disconnects for ops dashboards left open in a background tab.

### Notification topics (`notify/<scope>/<id>`)

Replay from `Last-Event-ID` with TTL. Notifications have a shelf life --
a "deployment started" from two hours ago is not actionable. Use
`PublishWithTTL` so stale notifications auto-expire from the replay cache.

```go
broker.SetReplayPolicy("notify/user/alice", 50)
broker.PublishWithTTL("notify/user/alice", renderNotification(n), 30*time.Minute)
```

On gap, the replay cache already discards expired entries, so
`SubscribeFromID` returns only what is still relevant.

---

## Gap handling

A gap occurs when the client sends `Last-Event-ID` but the server's replay
cache does not have it. The ID rolled out of the ring buffer, the server
restarted, or a `ReplayStore` was not configured. This is normal.

Use `OnReplayGap` to observe gaps (the callback fires in its own goroutine)
and `SetReplayGapPolicy` to define fallback behavior:

- `GapSilent` -- no replay, subscriber receives only live messages.
  Backwards compatible but not honest.
- `GapFallbackToSnapshot` -- call the snapshot function and deliver it
  as the first message, preceded by a `tavern-replay-gap` control event.
  Honest.

**Recommended fallbacks by topic category:**

| Category | Fallback | Rationale |
|----------|----------|-----------|
| Resource | Fresh snapshot | The client wants current state anyway |
| Collection | Full collection snapshot | Partial deltas with unknown gaps corrupt the view |
| Presence | Current presence state | Stale join/leave events are misleading |
| Admin | Current status snapshot | Ops dashboards need current truth |
| Notification | Unexpired notifications only | TTL discards what is no longer actionable |

When in doubt, snapshot. A fresh snapshot is always honest. A partial
replay that skips unknown gaps is not.

---

## Reconnection UX

**Bundled replay.** Use `SetBundleOnReconnect` on topics where replay may
deliver many messages at once. Bundling batches replayed messages into a
single delivery, reducing DOM churn on reconnect. For HTMX, bundled OOB
swaps are cleaner than N individual swaps arriving in rapid succession.

```go
broker.SetBundleOnReconnect("collection/tasks", true)
```

**Reconnect notification.** Use `OnReconnect` to fire a callback when a
subscriber reconnects with a `Last-Event-ID`. Log reconnection events,
update metrics, or send a welcome-back control event.

```go
broker.OnReconnect("collection/tasks", func() {
    log.Info("subscriber reconnected to collection/tasks")
})
```

---

## Snapshot providers

`SubscribeWithSnapshot` calls your snapshot function on subscribe and
delivers the result as the first message. The function should be fast
(it runs synchronously during subscription setup), return the current
representation (not compute history), and return empty string to skip.

What to return: resource topics render the current entity, collection
topics render the current list, presence topics render the current
participant list, admin topics render current system status, notification
topics render only unexpired notifications.

The snapshot function in `SubscribeWithSnapshot` and the one passed to
`SetReplayGapPolicy` can be the same function. They answer the same
question: "what is the current state?".

---

## Anti-patterns

**Replaying stale presence events.** A join from 10 minutes ago for a user
who already left is worse than no event. Presence topics should snapshot,
never replay.

**Relying solely on replay without a snapshot fallback.** Ring buffers roll
over. Servers restart. Without a snapshot fallback, a gap means the client
gets nothing.

**Using replay as a durable event log.** The replay cache is a ring buffer,
not a journal. If you need durable ordered history, that belongs in your
application's persistence layer.

**Treating gaps as errors.** Gaps are normal. The replay buffer has a finite
size. A well-configured system detects gaps and falls back to snapshots.

**Oversized replay buffers as a substitute for snapshots.** A 10,000-entry
replay buffer does not eliminate gaps -- it just makes them rarer and harder
to test for. Use a right-sized buffer with a snapshot fallback.
