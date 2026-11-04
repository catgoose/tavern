# Choosing a Subscription

Tavern has many subscribe methods because delivery needs differ. Most
applications need one of three patterns. Pick by what a lost message costs
the client, then check your options with `ValidateSubscribeOptions`.

| The client needs | Pattern | Recovery |
|---|---|---|
| The current value; old values are worthless | [Latest representation](#latest-representation) | None: the next value replaces it |
| To know something changed, every time | [Distinct invalidations](#distinct-invalidations) | Refetch canonical state when the stream ends |
| The events themselves, in order, across reconnects | [ID-backed replay](#id-backed-replay) | Resume with `Last-Event-ID`; refetch on a gap |

Delivery is best-effort in every pattern: a full subscriber queue drops
messages rather than blocking `Publish`. The patterns differ in how a client
finds out and recovers.

---

## Latest representation

Publish the whole current state of a region (a rendered fragment, a gauge, a
price) and let newer values replace older ones.

```go
ch, unsub := broker.SubscribeWithCoalescing("prices")
defer unsub()
return tavern.StreamSSE(r.Context(), w, ch, func(s string) string { return s },
    tavern.WithStreamSnapshot(renderPrices),
    tavern.WithStreamWriteTimeout(10*time.Second),
)
```

- Coalescing keeps one latest value **per subscription**, not per key. Two
  regions published on one topic overwrite each other; give each region its
  own topic, or use distinct invalidations instead.
- `SubWithRate` also limits update frequency, but a held value flushed into a
  full queue is dropped, so the final value can be lost. Prefer coalescing
  when the last value must arrive.
- `SubWithSnapshot` (or `WithStreamSnapshot`) sends the initial state first.

## Distinct invalidations

Publish small "this changed" messages and let the client fetch the canonical
state. Loss matters here: a dropped invalidation leaves a stale view, so end
the stream on loss instead of continuing silently.

```go
status := tavern.NewSubscriptionStatus()
ch, unsub := broker.SubscribeWith("orders",
    tavern.SubWithScope(userID),           // derived from the authenticated session
    tavern.SubWithDisconnectOnLoss(),
    tavern.SubWithStatus(status),
)
if ch == nil {
    http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
    return
}
defer unsub()
err := tavern.StreamSSE(r.Context(), w, ch, func(s string) string { return s },
    tavern.WithStreamSnapshot(func() string { return versionFrame(userID) }),
    tavern.WithStreamWriteTimeout(10*time.Second),
    tavern.WithStreamWriteErrors(),
)
logStreamEnd(status.Termination(), err) // e.g. queue-loss, broker-closed
```

When the stream ends the browser reconnects, and the client must
reconcile. A canonical GET followed by a new subscription does **not** close
the race: a change between the two is missed. Two sound sequences, both
owned by the application:

- **Subscribe, then fetch.** Treat the stream as open once its first frame
  arrives (send one with a snapshot), fetch canonical state, and apply
  invalidations that arrived during the fetch by fetching again.
- **Versioned state.** Include a version or cursor in canonical responses
  and in each invalidation, and ignore invalidations at or below the version
  you already have. The snapshot frame can carry the current version.

`SubscribeMultiWith(topics, SubWithDisconnectOnLoss(), ...)` applies this to
several topics; loss on any topic ends the whole aggregate, and its status
names the topic that ended it.

## ID-backed replay

Publish events with IDs and keep a bounded history so a reconnecting client
resumes where it left off.

```go
broker.SetReplayPolicy("activity", 100)
broker.SetReplayGapPolicy("activity", tavern.GapFallbackToSnapshot, renderActivity)
broker.PublishWithID("activity", eventID, fragment)

mux.Handle("/sse/activity", broker.SSEHandler("activity",
    tavern.WithWriteTimeout(10*time.Second)))
```

- Retention is by count. A client away longer than `n` events gets a gap:
  `tavern-replay-gap` and, with `GapFallbackToSnapshot`, a fresh snapshot.
- Replay goes through the subscriber buffer; overflow on a resume is
  reported as `tavern-replay-truncated`.
- With a `ReplayStore`, add `SubWithTerminateOnReplayError` (via
  `SubscribeFromIDWith`) so a store failure ends the stream instead of
  looking like a gap or a successful resume.
- One `EventSource` has one `Last-Event-ID`. On a multi-topic stream every
  topic resumes from that single ID, so topics that do not contain it are
  treated as gaps. Use separate streams or your own per-topic cursors when
  each topic needs its own position.

---

## Option compatibility

✓ supported · — no effect (accepted and ignored) · ✗ panics

| Option | `SubscribeWith` | `SubscribeFromIDWith` | `SubscribeMultiWith` | `SubscribeGlobWith` |
|---|---|---|---|---|
| `SubWithScope` | ✓ | ✓ live only (replay log is unscoped) | ✓ every topic | ✓ |
| `SubWithFilter` | ✓ | ✓ replay and live | ✓ every topic | ✓ |
| `SubWithRate` | ✓ latest-wins | ✓ live only | ✓ every topic | ✓ drops extra messages |
| `SubWithMeta` | ✓ | ✓ | ✓ every topic | — |
| `SubWithSnapshot` | ✓ first message | ✓ fresh subscribe only | ✓ every topic | — |
| `SubWithDisconnectOnLoss` | ✓ | ✓ live only | ✓ and ends the aggregate | ✗ |
| `SubWithTerminateOnInnerClose` | — | — | ✓ | — |
| `SubWithTerminateOnReplayError` | ✓ when it reads the store | ✓ | ✓ every topic | ✗ |
| `SubWithStatus` | ✓ | ✓ | ✓ aggregate cause | ✗ |
| `SubWithDisconnectOnLoss` + `SubWithRate` | ✗ | ✗ | ✗ | ✗ |

Reusing a `SubscriptionStatus` panics. The legacy methods (`Subscribe`,
`SubscribeScoped`, `SubscribeWithFilter`, `SubscribeWithSnapshot`,
`SubscribeWithRate`, `SubscribeWithMeta`, `SubscribeWithCoalescing`,
`SubscribeFromID`, `SubscribeMulti`, `SubscribeGlob`) take no options; each
behaves like the matching composable call without status or loss-awareness.
`SubscribeWithCoalescing` has no composable equivalent.

### Validating options

```go
opts := []tavern.SubscribeOption{tavern.SubWithDisconnectOnLoss(), tavern.SubWithRate(rate)}
if err := tavern.ValidateSubscribeOptions(tavern.TargetSubscribeWith, opts...); err != nil {
    // tavern: SubscribeWith: SubWithRate: conflicting options: ...
}
```

`ValidateSubscribeOptions` returns one `*OptionError` per problem, joined.
Each wraps a sentinel:

| Error | Meaning | The subscribe call |
|---|---|---|
| `ErrOptionConflict` | Options rejected together | panics |
| `ErrOptionUnsupported` | Option not supported by this target | panics |
| `ErrOptionInvalid` | Nil option, unknown target, or a status already attached | panics (nil option, attached status) |
| `ErrOptionIgnored` | Option or value with no effect (empty scope, nil filter/snapshot/status, zero rate, target-specific no-ops) | accepts it |

Validation has no side effects: it does not run snapshot or filter
functions, touch the broker, store or backend, or attach the status, so the
same options can be validated repeatedly and then used once. It does not
reserve anything; a status can still be attached elsewhere between
validation and your subscribe call.

---

## Differences between variants

These follow from existing contracts and are kept as they are.

- **Replay on a fresh subscribe.**
  - `SubscribeWith` without filter or snapshot replays the replay cache, or
    the `ReplayStore`'s latest entries, as plain messages.
  - With a filter it replays the in-memory cache, filtered; a `ReplayStore`
    is not consulted.
  - With a snapshot and no filter it sends the snapshot and no replay.
  - `SubscribeFromIDWith` (and loss-aware `SubscribeWith`) replays the
    ID-backed log with `id:` lines, after the snapshot when there is one.
- **Snapshots** are queued before the subscription becomes visible, so they
  always arrive first. `SubscribeFromIDWith` sends one only when the
  `Last-Event-ID` is empty.
- **Filters** apply to replay and live messages. Control events
  (`tavern-reconnected`, `tavern-replay-gap`, `tavern-replay-truncated`)
  bypass them.
- **Scopes.** `PublishWithID` reaches only unscoped subscribers; scoped
  subscribers receive live messages from `PublishTo`.
- **Aggregates.** By default `SubscribeMultiWith` stays open until every
  topic ends; `SubWithTerminateOnInnerClose` or `SubWithDisconnectOnLoss`
  makes every topic required.
- **Glob rate limiting** drops messages faster than the rate instead of
  holding the latest.

See [snapshot-replay.md](snapshot-replay.md), [topic-semantics.md](topic-semantics.md),
[page-multiplexing.md](page-multiplexing.md) and
[delivery-observability.md](delivery-observability.md) for details, and
[production-operations.md](production-operations.md) for deployment.

## Application responsibilities

Tavern delivers messages; the application owns authorization (derive scopes
and topics from the authenticated identity before subscribing), canonical
state, conflict and edit protection, and business-level recovery. See
[multi-user-coordination.md](multi-user-coordination.md) for versioned
writes, claims and draft-safe recovery.
