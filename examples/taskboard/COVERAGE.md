# Showcase coverage

What the task board and its showcase demonstrate, for Tavern and for the
pinned tavern-js release. Each row names a capability family and the
variants it covers, where it runs, the evidence, and its status.

Status:

- **Browser**: runs in the app and is asserted by the browser tests
  (`browser/tests/`).
- **App**: runs in the app and is asserted by the Go tests that drive it
  over HTTP (`*_test.go` in this directory).
- **Exercise**: covered by another runnable example or smoke test, not by a
  showcase page.
- **Partial**: some named variants are covered; the rest are planned.
- **Planned (pass N)**: not demonstrated yet; the pass in
  [roadmap-002](../../plans/roadmap-002-tavern-and-tavern-js-feature-showcase.md)
  that will cover it.

A library unit test alone does not count as a demonstration here.

Scenarios: **Board** is `/` (one invalidation stream per window, slow
connection demo). **Connection** is `/showcase/connection` (an ID-backed
counter with tavern-js connection state). **Collaboration** is
`/showcase/collaboration` (two personas on a shared board with OOB updates,
per-persona delivery, a topic group and a bulk action; `collab*.go`).

## Tavern

| Capability | APIs and variants | Scenario | Evidence | Status |
|---|---|---|---|---|
| Broker setup | `NewSSEBroker`, `WithBufferSize`, `WithLogger`; `WithDropOldest`, `WithKeepalive` | Board (`app.go`) | Board tests | Partial: `WithDropOldest`, `WithKeepalive` planned (pass 005, 007) |
| Admission | `WithMaxSubscribers`; `WithMaxSubscribersPerTopic`, `WithAdmissionControl` | Board: 503 when full | `TestAdmissionLimit` | Partial: per-topic and custom admission planned (pass 007) |
| Plain publish | `Publish`, `NewSSEMessage` | Board invalidations | `TestMutationsReachEveryViewer`; browser board test | Browser |
| Scoped publish | `PublishTo`, `SubWithScope`; `SubscribeScoped`, `ScopedTopic` | Collaboration: each persona's stream is scoped to that persona; board changes and inbox notices are published to the scopes allowed to see them | `TestCollabCRUDReachesEachViewer`, `TestCollabInboxIsScopedAndFiltered`; `browser/tests/collab.spec.js` | Partial: `SubscribeScoped` and `ScopedTopic` planned (pass 004) |
| Blocking publish | `PublishBlocking`, `PublishBlockingTo` | — | — | Planned (pass 006) |
| ID-backed replay | `PublishWithID`, `SetReplayPolicy`, `SubscribeFromIDWith` | Connection: fresh streams replay the kept values; a reconnect resumes after `Last-Event-ID` | `TestCounterReachesEveryViewer`, `TestDropReplaysWhatTheWindowMissed`; browser reconnect test; capacity reconnect burst | Browser |
| Reconnect confirmation | `tavern-reconnected` with `replayDelivered`/`replayDropped` | Connection: drop, then increment while away | Browser reconnect test asserts `replayDelivered: 1` | Browser |
| Gaps and truncation | `SetReplayGapPolicy` (`GapSilent`, `GapFallbackToSnapshot`), `OnReplayGap`, `tavern-replay-gap`, `tavern-replay-truncated` | Capacity counts gaps on its reconnect burst | `examples/capacity` | Exercise; showcase planned (pass 005) |
| Reconnect hooks | `OnReconnect`, `SetBundleOnReconnect`, `SubscribeMultiFromID` | — | — | Planned (pass 005) |
| Replay storage | `WithReplayStore`, `ReplayStore`, `NewMemoryReplayStore`, `WithReplayStoreErrorHandler`, `ReplayStoreError`, `SubWithTerminateOnReplayError` | — | — | Planned (pass 005) |
| Snapshots and latest-only replay | `SubWithSnapshot`, `PublishWithReplay`; `SubscribeWithSnapshot`, `ClearReplay` | Collaboration: the board stream starts from a snapshot taken under the board's lock; the feed's activity and counts are kept with `PublishWithReplay`, so a new feed stream starts from the latest of each | `TestCollabSnapshotsFollowVisibility`, `TestCollabChangeDuringSnapshotIsNotLost`, `TestCollabReconnectStartsFromCanonicalState`, `TestCollabFeedIsADynamicGroup`; `browser/tests/collab.spec.js` | Partial: `SubscribeWithSnapshot`, `ClearReplay` planned (pass 005) |
| Loss-aware invalidation | `SubscribeWith`, `SubWithDisconnectOnLoss`, `SubWithStatus`, `NewSubscriptionStatus`, `Termination` reasons | Board: paused window overflows, stream ends, reconnect reloads. Collaboration streams use the same options and rebuild from a snapshot on reconnect | `TestLossEndsStreamAndReconnectRefetches`, `TestNonReadingClientIsReleased`; proxy smoke | App |
| Option validation | `ValidateSubscribeOptions`, `OptionError`, `SubscribeTarget` | — | — | Planned (pass 008) |
| Filters, rate, metadata | `SubWithFilter`; `SubWithRate`, `SubWithMeta`, `SubscribeWithFilter`, `SubscribeWithRate`, `SubscribeWithMeta`, `SubscribeWithCoalescing`, `SubscribeScopedWithFilter`, `SubscribeScopedWithRate`, `SubscribeScopedWithCoalescing` | Collaboration: a contributor's stream filters out "completed" notices; the predicate reads the app-generated SSE event name, not the HTML | `TestCollabInboxIsScopedAndFiltered` | Partial: rate, metadata and coalescing planned (pass 006) |
| Multi-topic and glob | `SubscribeMulti`, `SubscribeMultiWith`, `SubscribeMultiWithMeta`, `SubWithTerminateOnInnerClose`, `SubscribeGlob`, `SubscribeGlobScoped`, `SubscribeGlobWith`, `UnsubscribeGlob`, `TopicMessage` | — | — | Planned (pass 004) |
| Groups | `DynamicGroup`, `DynamicGroupHandler`; `DefineGroup`, `GroupHandler` | Collaboration feed: one stream combining the persona's role's activity and counts topics, chosen on the server; event names are the topic names. The group stream is not loss-aware, so the app republishes the feed after a drop | `TestCollabFeedIsADynamicGroup`; `browser/tests/collab.spec.js` | Partial: static groups planned (pass 004) |
| Dynamic topics | `AddTopic`, `RemoveTopic`, `AddTopicForScope`, `RemoveTopicForScope`, `SubscriberTopics`, `tavern-topics-changed` | — | — | Planned (pass 004) |
| Custom streams | `StreamSSE`, `WithStreamSnapshot`, `WithStreamHeartbeat`, `WithStreamWriteTimeout`, `WithStreamWriteErrors`; `WithStreamWriter` | Board and Connection streams | Board and showcase tests; proxy smoke (heartbeat across the proxy idle timeout) | Partial: `WithStreamWriter` planned (pass 007) |
| Turnkey handler | `SSEHandler`, `WithWriteTimeout`, `WithWriteErrorHandler`, `WithMaxConnectionDuration`, `WithReconnectDelay`, `WithSSEWriter`, `WriteError` | — | — | Planned (pass 007) |
| Retry hints and shutdown | `SetRetryAll`, `SetRetry`, `SSEMessage.WithRetry`, `Close` | Board: shutdown sends `retry:` and ends streams | `TestShutdownSendsRetryAndEndsStreams`; proxy smoke (SIGTERM) | Partial: `SetRetry` per topic and `WithRetry` planned (pass 007) |
| Disconnecting a subscriber | `Disconnect`, `Subscribers`, `SubscriberInfo` | — | — | Planned (pass 007) |
| OOB fragments | `Replace`, `Append`, `Prepend`, `Delete`, `ReplaceComponent`, `AppendComponent`, `RenderFragments`, `RenderComponent`, `Component`, `PublishOOBTo` (through `PublishLazyOOBTo` and `Batch`); `PrependComponent`, `RenderComponentErr`, `PublishOOB` | Collaboration: new cards are appended, edits replace the card, moves delete and append, deletes remove it; notices are prepended and the oldest deleted; snapshots replace each column. Cards are `Component` values rendered with html/template | `TestCollabCRUDReachesEachViewer`, `TestCollabInboxIsScopedAndFiltered`; `browser/tests/collab.spec.js` | Partial: `PrependComponent`, `RenderComponentErr`, `PublishOOB` planned (pass 006) |
| Lazy and change-only publish | `PublishLazyOOBTo` (Collaboration card changes render only while someone watches the board); `PublishLazyOOB`, `PublishIfChanged`, `PublishIfChangedTo`, `PublishIfChangedOOB`, `PublishIfChangedOOBTo`, `PublishLazyIfChangedOOB`, `PublishLazyIfChangedOOBTo`, `ClearDedup` | Collaboration (`PublishLazyOOBTo`) | `TestCollabCRUDReachesEachViewer` | Partial: the rest planned (pass 006) |
| Debounce and throttle | `PublishDebounced`, `PublishThrottled` | — | — | Planned (pass 006) |
| TTL | `PublishWithTTL`, `PublishToWithTTL`, `PublishOOBWithTTL`, `PublishIfChangedWithTTL`, `WithAutoRemove`, `WithMessageTTLSweep`, `WithTopicTTL` | — | — | Planned (pass 006). TTL removes an entry from the replay cache so later subscribers do not receive it; live delivery is immediate. `WithAutoRemove` also sends a delete fragment to connected clients when the entry expires, on the sweep interval. |
| Batching | `Batch`, `PublishBatch.PublishOOBTo`, `Flush`; `Publish`, `PublishTo`, `PublishWithID`, `PublishWithTTL`, `PublishOOB`, `Discard` | Collaboration: **Clear finished tasks** queues each viewer's removals and flushes them as one event per viewer | `TestCollabBulkClearIsOneBatch`; `browser/tests/collab.spec.js` | Partial: the other batch methods planned (pass 006). `Flush` concatenates the batch's messages per topic and scope into one write per subscriber. It is not a transaction, and messages for different topics are separate writes. A filter sees a batched message as one message, so the collaboration scenario never batches inbox notices. |
| Ordering | `SetOrdered` | — | — | Planned (pass 006). Serializes concurrent publishes to one topic so every subscriber sees the same order. |
| Backpressure and slow subscribers | `WithAdaptiveBackpressure`, `AdaptiveBackpressure`, `BackpressureTier`, `OnBackpressureTierChange`, `SetSimplifiedRenderer`, `tavern-backpressure`, `WithSlowSubscriberEviction`, `WithSlowSubscriberCallback`, `OnPublishDrop`, `PublishDrops` | Board: a slow window ends on overflow (loss-aware, not adaptive). Collaboration: a paused feed overflows and `OnPublishDrop` triggers a republish of the current feed | Capacity reports drops; `TestCollabFeedRecoversAfterDroppedUpdate`, `TestCollabFeedStaysStaleWithoutRepair`; `browser/tests/collab.spec.js` | Partial: adaptive tiers and eviction planned (pass 005, 006) |
| Hooks and middleware | `After`, `OnMutate`, `NotifyMutate`, `MutationEvent`, `Use`, `UseTopics`, `Middleware`, `OnFirstSubscriber`, `OnLastUnsubscribe` | — | — | Planned (pass 006) |
| Scheduled publishing | `RunPublisher`, `NewScheduledPublisher`, `Register`, `SetInterval`, `Start`, `WithBaseTick`, `SectionOptions`, `CircuitBreakerConfig`, `OnRenderError`, `RenderError` | — | — | Planned (pass 006) |
| Introspection and metrics | `HasSubscribers`, `SubscriberCount`, `TopicCounts`, `Stats`, `WithMetrics`, `Metrics`, `WithObservability`, `Observability`, `WithConnectionEvents` | Go tests read `HasSubscribers` and `SubscriberCount` | Board tests; capacity | Partial: an operations view planned (pass 007) |
| Backends | `WithBackend`, `backend.Backend`, `HealthAwareBackend`, `ObservableBackend`, `backend/memory` | — | — | Planned (pass 007) |
| Presence | `presence.Tracker` (`Join`, `Leave`, `Update`, `Heartbeat`, `List`, `Get`) | — | — | Planned (pass 007) |
| Test helpers | `taverntest` (`Capture`, `Recorder`, `SSERecorder`, `MockBroker`, `SimulatedConnection`, `SlowSubscriber`) | — | — | Planned (pass 008) |
| Metrics adapters | `tavernprom`, `tavernotel` modules | — | Their own module tests | Planned (pass 007) |
| Proxy deployment | nginx configuration, heartbeats, shutdown through the proxy | `deploy/` | `make proxy-smoke` in CI | Exercise |
| Capacity | broker and HTTP load presets | `examples/capacity` | `make capacity` | Exercise |

## tavern-js 0.0.23

| Capability | Attributes, events and API | Scenario | Evidence | Status |
|---|---|---|---|---|
| Discovery | automatic binding of `sse-connect` elements on load | Connection | Browser tests | Browser |
| Dynamic binding and teardown | `MutationObserver` binding, `Tavern.bind`, `init`, `scanAndBind`, `destroy` | — | — | Planned (pass 004) |
| Connection state | `tavern-live-class`, `tavern-reconnecting-class`, `tavern-status`, `tavern-status-live`, `tavern-status-recovering` | Connection | Browser tests assert the classes and the live and lost status elements; the recovering element is configured but shown too briefly to assert | Browser |
| Stale state | `tavern-stale-class`, `tavern-status-stale`, `tavern:stale` | Connection (configured; this scenario never goes stale) | — | Planned (pass 005) |
| Lifecycle events | `tavern:live`, `tavern:disconnected`, `tavern:recovering`, `tavern:reconnected` (with replay counts), `tavern:transport-closed`, `tavern:transport-open` | Connection event log | Browser reconnect test asserts their order | Browser |
| Region updates | `tavern:region-updated`, `tavern-updated-class`, `tavern-updated-ms` | Connection | Browser test asserts the event; the class is configured, not asserted | Partial |
| Gap handling | `tavern-gap-action` (`reload`, `banner`, custom event), `tavern-gap-banner-text`, `tavern:replay-gap`, `tavern:replay-truncated` | — | — | Planned (pass 005) |
| Topic changes | `tavern:topics-changed` | — | — | Planned (pass 004) |
| Degradation | `tavern:stream-degraded`, `tavern:stream-restored` | — | — | Planned (pass 006) |
| Commands | `Tavern.command`, `tavern-command-delegate`, `tavern-command-target`, `tavern-command-dedup`, URL token expansion, `tavern:command-sent`, `-success`, `-error`, `tavern-hearth` | — | — | Planned (pass 003) |
| Interaction protection | `tavern-hot-policy` (`pause-on-pointerdown`, `defer-on-focus`), `tavern:policy-activated`, `tavern:policy-deactivated` | — | — | Planned (pass 003; [tavern-js#50](https://github.com/catgoose/tavern-js/issues/50), [tavern-js#53](https://github.com/catgoose/tavern-js/issues/53)) |
| Lifeline and scoped streams | `tavern-role`, `tavern-scope`, `tavern:stream-warming`, `-ready`, `-promoted`, `-retired`, `-fallback`, `Tavern.lifeline`, `stream`, `streams`, `promote`, `retire` | — | — | Planned (pass 004) |
| Debug logging | `tavern-debug` | — | — | Planned (pass 008) |

## Notes

- A fresh connection is marked live when its transport opens, before any
  server message. After a drop it becomes live only when the server sends
  `tavern-reconnected`. That event is sent only on a resume with
  `Last-Event-ID`, so tavern-js recovery status belongs on ID-backed
  streams. The board's invalidation stream has no event IDs and keeps its
  own ready/refetch status, so tavern-js is not loaded on the board.
- htmx logs `htmx:sseError` events with `console.error`, so the browser
  tests declare that one error on the page they drop.
