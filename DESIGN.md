# Design Note

## What Tavern is

Tavern is a live hypermedia delivery layer. It takes server-owned
representations and pushes them to clients over SSE. The server decides what
changes; Tavern delivers it honestly. That is the entire job.

## What belongs in core

These capabilities are central to Tavern's mission and belong in the main
package:

- **Topic-based pub/sub with concurrent-safe fan-out.** The fundamental
  primitive. Messages go to topics, subscribers receive them.
- **Replay, reconnection recovery, and gap honesty.** Clients disconnect.
  When they come back, Tavern catches them up or admits it cannot.
- **Delivery shaping for UI streams.** Debounce, throttle, coalesce, batch,
  backpressure. The server controls what reaches the client and when.
- **OOB fragment rendering.** Hypermedia swap targets for tools like HTMX.
  The server renders the HTML; Tavern delivers the fragments.
- **Observability of delivery health.** Publish counts, drop counts, replay
  stats, connection events. Delivery is the product, so delivery health is
  core.
- **SSE handler integration with topic groups.** Static and dynamic topic
  groups, single-connection multi-topic streams, Last-Event-ID handling.
- **Publish middleware and hooks.** Chaining server-side effects on publish,
  mutation signals, subscriber filtering.
- **Admission control and subscriber management.** Rate limiting, subscriber
  caps, connection lifecycle.

## What belongs outside core

These are legitimate needs but belong in adapters, companion packages, or
application code:

- **Specific backend implementations.** Redis, Postgres, NATS adapters
  implement the backend interface but ship separately.
- **Framework-specific integrations.** Beyond standard `net/http`, framework
  glue lives in examples or adapter packages.
- **Client-side JavaScript.** Beyond SSE's built-in `EventSource`, client
  code lives in companion libraries like tavern-js.
- **Application-level authorization.** Tavern provides admission hooks. Who
  gets in is your problem.
- **Persistence of application state.** Tavern delivers representations. It
  does not store your domain objects.

## What Tavern refuses to become

- A client-side state management framework.
- An offline-first sync engine or CRDT runtime.
- A generic event sourcing or event streaming platform.
- A full application protocol or RPC layer.
- A WebSocket replacement trying to be bidirectional.
- An SPA runtime that manages client-side DOM state.

If it pulls the client into owning state, it is out of scope.

## Evaluating new features

When a feature is proposed, ask:

1. Does it serve the "server owns the representation" model?
2. Does it improve delivery honesty -- replay, gaps, reconnection?
3. Does it help the server shape what reaches the client?
4. Does it keep the client thin and the server authoritative?
5. If it requires significant client-side logic, it probably does not belong.

A feature that passes these questions strengthens the core. A feature that
fails them may still be useful -- as an adapter, an example, or someone
else's library.

## On replay and gap handling

Replay and gap detection are honesty mechanisms. Replay exists so
reconnecting clients get caught up truthfully. Gap detection exists so the
server can admit when it cannot fill the hole. These are not caching
features. They are not performance optimizations. They exist because a
delivery layer that silently drops messages is lying.

## On observability

Tavern tracks delivery health because delivery is the product. Knowing that
messages were published is table stakes. Knowing they were delivered,
dropped, replayed, or truncated is what makes the system honest. Application
metrics belong in your application. Delivery metrics belong here.

## The short version

Server speaks, client listens. Tavern is the voice that carries.
