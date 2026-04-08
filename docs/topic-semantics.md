# Topic Semantics for Live Hypermedia

Tavern is a live hypermedia delivery layer. The server owns the
representation; Tavern delivers it honestly. Topics are how the server names
what it is talking about. This document defines the vocabulary for topic
shapes, scoping conventions, and subscription type selection.

These conventions are not enforced by the library. They are patterns that
make replay, filtering, observability, and multi-topic composition work
well together. Future features -- snapshot/replay semantics (#183) and page
multiplexing (#186) -- build directly on this vocabulary.

---

## Topic categories

### Resource topics

A single server-owned entity. The subscriber knows exactly what they are
watching.

Pattern: `resource/<type>/<id>`

Examples: `resource/tasks/42`, `resource/invoices/INV-2024-007`

Publishes carry the current representation of the resource. When the server
publishes to a resource topic, it is saying "this is what this thing looks
like now." Snapshot and replay policies attach naturally here -- a new
subscriber gets the current state, not a history lesson.

### Collection topics

A set of resources. Not the full set every time -- that is what snapshots
are for.

Pattern: `collection/<type>`

Examples: `collection/tasks`, `collection/orders`

Publishes carry deltas: additions, removals, reordering, count changes.
A collection topic tells subscribers "the set changed" and gives them enough
to update their view. The full set comes from a snapshot on connect or
from the initial HTTP response.

### Presence topics

Who is here. Tavern's `presence` package already handles the join/leave
lifecycle; these topics carry the events.

Pattern: `presence/<scope>`

Examples: `presence/tasks/42`, `presence/room/general`

Publishes carry join, leave, and update events. Presence topics are
inherently scoped to a context -- a document being edited, a room, a
resource being viewed. The scope appears in the topic path because it is
part of the identity.

### Admin and ops topics

System health, deployment status, queue depth, circuit breaker state.
Typically low-volume, high-importance.

Pattern: `admin/<concern>`

Examples: `admin/health`, `admin/deploys`, `admin/circuit-breakers`

These topics exist for internal dashboards and operational tooling. Glob
subscriptions (`admin/*` or `admin/**`) are natural here -- an ops view
wants everything under `admin/` without listing each concern individually.

### Notification topics

Per-user or per-tenant alerts. Best served by scoped subscriptions when the
topic name is shared but the content differs per viewer.

Pattern: `notify/<scope>/<id>`

Examples: `notify/user/alice`, `notify/org/acme`

When every user subscribes to `notify/user/<their-id>`, the topic path
carries the scope. When a shared topic like `notifications` delivers
different content per user, broker scoping (`SubscribeScoped`/`PublishTo`)
is the right tool. See the scoping conventions below.

---

## Scoping conventions

There are two ways to scope delivery: encode the scope in the topic path,
or use broker-level scoping. Do not mix both for the same concern.

**Topic path scoping** -- Use when the scope is part of the resource
identity. The topic name alone tells you who it is for.

```
resource/orgs/acme/projects/7    -- scoped by org and project
presence/room/general            -- scoped by room
notify/user/alice                -- scoped by user
```

This works when different scopes are genuinely different topics. A
subscriber to `resource/orgs/acme/projects/7` has no interest in project 8.
Glob patterns compose naturally: `resource/orgs/acme/**` gives you
everything in the org.

**Broker scoping** -- Use when the same topic name carries different content
per viewer. The topic is shared; the broker resolves who sees what.

```go
broker.SubscribeScoped("dashboard", tenantID)
broker.PublishTo("dashboard", tenantID, fragment)
```

This works when the topic represents a shared concept (everyone has a
dashboard) but the content is tenant-specific or user-specific. The topic
name stays clean for metrics and logging. Replay and snapshot policies
apply per scope automatically.

**When to choose which:**

- If two users subscribing to the same topic should see the same content,
  use topic path scoping.
- If two users subscribing to the same topic should see different content,
  use broker scoping.
- If you find yourself encoding session IDs or auth tokens into topic
  names, you want broker scoping instead.

---

## Subscription type guidance

| Type | When to use |
|------|-------------|
| `Subscribe` | Single topic, simple case. One resource, one stream. |
| `SubscribeScoped` | Same topic name, different content per scope. Per-user notifications, per-tenant dashboards. |
| `SubscribeMulti` | Page needs several specific topics. A dashboard watching `cpu`, `memory`, and `disk` on one channel. |
| `SubscribeGlob` | Monitoring or admin views that span many resources. `sensors/*`, `admin/**`. Use when the subscriber wants a class of topics, not a specific list. |
| `DefineGroup` / `GroupHandler` | Every connection to this handler gets the same set of topics. Static dashboard pages. |
| `DynamicGroup` / `DynamicGroupHandler` | Topics depend on request context -- auth, route params, feature flags. Per-user topic sets resolved at connection time. |

For page-level composition, prefer `SubscribeMulti` or topic groups over
opening multiple `EventSource` connections. A single SSE connection carrying
tagged messages is cheaper than N connections carrying untagged ones.

---

## Naming conventions

- Use `/` as the separator. This aligns with glob pattern matching and
  reads naturally in hierarchical structures.
- Lowercase, hyphen-separated segments: `resource/task-lists/7`, not
  `Resource/TaskLists/7`.
- Be specific enough that observability tools can filter usefully. A topic
  named `updates` tells you nothing in a metric dashboard. A topic named
  `resource/orders/42` tells you exactly what changed.
- Topic names appear in logs, metrics, and SSE event streams. They should
  be readable by humans debugging at 2am.

---

## Anti-patterns

**Volatile state in topic names.** Encoding session IDs, timestamps, or
request IDs into topic names creates unbounded topic cardinality. Replay
caches, metrics, and glob indexes all suffer. Use broker scoping for
per-session delivery.

**Topics so generic they require client-side filtering.** A topic named
`events` that carries every event type forces the client to inspect and
discard. This violates the "server owns the representation" principle -- the
server should publish to specific topics so clients subscribe to exactly
what they need.

**One mega-topic with type discrimination.** Publishing `{"type": "order",
"action": "created", ...}` and `{"type": "user", "action": "updated", ...}`
to the same `all-events` topic pushes routing logic to the client. The
server knows the structure; it should use it to choose the topic.

**Mixing path scoping and broker scoping for the same concern.** If
`notify/user/alice` is a topic and you also use `PublishTo("notify",
"alice", ...)`, you have two delivery paths for the same concept. Pick one.

---

## Summary

Topics are names for what the server is talking about. The five categories
-- resource, collection, presence, admin, notification -- cover the common
shapes. Path scoping and broker scoping are complementary but should not
overlap. Subscription types exist to match how the client wants to listen
to what the server is already publishing.

The server speaks. Topics are the words it uses. Tavern carries them.
