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
**Lab** is `/showcase/lab` (tavern-js commands and interaction protection
over three live regions, live editing and stale-save refusal; `lab*.go`).
**Shell** is `/showcase/shell` (a lifeline that stays connected while views
with their own streams are swapped in and out; server-side topic changes;
multi-topic, glob and group subscriptions; `shell*.go`). **Recovery** is
`/showcase/recovery` (resume, gaps, truncation, full queues and replay
store failures, each on its own broker; `recovery*.go`).

## Tavern

| Capability | APIs and variants | Scenario | Evidence | Status |
|---|---|---|---|---|
| Broker setup | `NewSSEBroker`, `WithBufferSize`, `WithLogger`; `WithDropOldest`, `WithKeepalive` | Board (`app.go`). Recovery: a drop-oldest broker with queues of four keeps the newest values | Board tests; `TestRecoveryFullQueues`; `browser/tests/recovery.spec.js` | Partial: `WithKeepalive` planned (pass 007) |
| Admission | `WithMaxSubscribers`; `WithMaxSubscribersPerTopic`, `WithAdmissionControl` | Board: 503 when full | `TestAdmissionLimit` | Partial: per-topic and custom admission planned (pass 007) |
| Plain publish | `Publish`, `NewSSEMessage` | Board invalidations | `TestMutationsReachEveryViewer`; browser board test | Browser |
| Scoped publish | `PublishTo`, `SubWithScope`, `SubscribeScoped` (the legacy form of `SubscribeWith` with `SubWithScope`); `ScopedTopic` | Collaboration: each persona's stream is scoped to that persona. Lab: each region stream is scoped to its connection. Shell: role notes reach the lifelines of one role | `TestCollabCRUDReachesEachViewer`, `TestCollabInboxIsScopedAndFiltered`, `TestShellLeadsChannelAndNotesFollowRole`; `browser/tests/collab.spec.js`, `browser/tests/shell.spec.js` | Partial: `ScopedTopic` only builds colon-separated names (see the README); it is not used here. Broker scopes select delivery; slash paths are what globs match; neither grants access |
| Blocking publish | `PublishBlocking`, `PublishBlockingTo` | — | — | Planned (pass 006) |
| ID-backed replay | `PublishWithID`, `SetReplayPolicy`, `SubscribeFromIDWith` | Connection: fresh streams replay the kept values; a reconnect resumes after `Last-Event-ID` | `TestCounterReachesEveryViewer`, `TestDropReplaysWhatTheWindowMissed`; browser reconnect test; capacity reconnect burst | Browser |
| Reconnect confirmation | `tavern-reconnected` with `replayDelivered`/`replayDropped` | Connection: drop, then increment while away | Browser reconnect test asserts `replayDelivered: 1` | Browser |
| Gaps and truncation | `SetReplayGapPolicy` (`GapSilent`, `GapFallbackToSnapshot`), `OnReplayGap`, `tavern-replay-gap`, `tavern-replay-truncated` | Recovery: an absence beyond retention is a gap answered with the gap event and a snapshot, or, with `GapSilent`, continued with an old value that only the server knows is behind. A replay larger than the queue is cut short; its reconnected and truncated notices are dropped with it, so the server's `OnReconnect` counts are the record and a republish repairs the value. Capacity counts gaps on its reconnect burst | `TestRecoveryLongAbsenceIsAGapWithASnapshot`, `TestRecoveryGapSnapshotIsNotOvertaken`, `TestRecoverySilentGapLeavesTheWindowBehind`, `TestRecoveryTruncatedReplayIsRepaired`; `browser/tests/recovery.spec.js`; `examples/capacity` | Browser. `tavern-replay-truncated` is only sent when it fits after the replay, which a full queue rules out; it is asserted absent, not received |
| Reconnect hooks | `OnReconnect`, `SetBundleOnReconnect`, `SubscribeMultiFromID` | Recovery: `OnReconnect` records missed, replayed and dropped counts per window; one topic's replay is bundled into one write; `SubscribeMultiFromID` resumes two topics from one ID, each confirming and reporting gaps on its own, with no order between them | `TestRecoveryResumeReplaysAShortAbsence`, `TestRecoveryMultiResumesEachTopic`; `browser/tests/recovery.spec.js` | Browser |
| Replay storage | `WithReplayStore`, `ReplayStore`, `NewMemoryReplayStore`, `WithReplayStoreErrorHandler`, `ReplayStoreError`, `SubWithTerminateOnReplayError` | Recovery: a `MemoryReplayStore` wrapped so appends and reads can fail. A failed read is a gap with a snapshot; failed appends are lost without a gap; strict mode ends the subscription with `replay-failure` and the app continues from a snapshot; replay resumes once the fault clears; a window behind failed appends recovers after Clear replay history and Reconnect now, with no further change. In memory, so it does not survive a restart | `TestRecoveryReplayStoreFailures`; `browser/tests/recovery.spec.js` | Browser |
| Snapshots and latest-only replay | `SubWithSnapshot`, `PublishWithReplay`; `SubscribeWithSnapshot`, `ClearReplay` | Collaboration: the board stream starts from a snapshot taken under the board's lock; the feed's activity and counts are kept with `PublishWithReplay`, so a new feed stream starts from the latest of each | `TestCollabSnapshotsFollowVisibility`, `TestCollabChangeDuringSnapshotIsNotLost`, `TestCollabReconnectStartsFromCanonicalState`, `TestCollabFeedIsADynamicGroup`; `browser/tests/collab.spec.js` | Browser. Recovery: `SubscribeWithSnapshot` is the strict store scenario's way back after a failed replay; `ClearReplay` drops stored history and the topic's replay policies, which the app sets again (`TestRecoveryReplayStoreFailures`) |
| Loss-aware invalidation | `SubscribeWith`, `SubWithDisconnectOnLoss`, `SubWithStatus`, `NewSubscriptionStatus`, `Termination` reasons | Board: paused window overflows, stream ends, reconnect reloads. Collaboration streams use the same options and rebuild from a snapshot on reconnect | `TestLossEndsStreamAndReconnectRefetches`, `TestNonReadingClientIsReleased`; proxy smoke | App |
| Option validation | `ValidateSubscribeOptions`, `TargetSubscribeMultiWith`; `OptionError`, other targets | Shell: the board view's options are validated once at startup | `newShell` panics on an invalid combination | Partial: a failing validation and `OptionError` inspection planned (pass 008) |
| Filters, rate, metadata | `SubWithFilter`; `SubWithRate`, `SubWithMeta`, `SubscribeWithFilter`, `SubscribeWithRate`, `SubscribeWithMeta`, `SubscribeWithCoalescing`, `SubscribeScopedWithFilter`, `SubscribeScopedWithRate`, `SubscribeScopedWithCoalescing` | Collaboration: a contributor's stream filters out "completed" notices; the predicate reads the app-generated SSE event name, not the HTML | `TestCollabInboxIsScopedAndFiltered` | Partial: rate, metadata and coalescing planned (pass 006) |
| Multi-topic and glob | `SubscribeMultiWith` (snapshot, loss-aware, status), `SubscribeMultiWithMeta`, `SubscribeGlobWith`, `TopicMessage`; `SubscribeMulti` (used by `GroupHandler`); `SubWithTerminateOnInnerClose`, `SubscribeGlob`, `SubscribeGlobScoped`, `UnsubscribeGlob` | Shell: the board view is one stream over two topics, with one snapshot written before any message (a per-topic `SubWithSnapshot` would queue a copy per topic, which can arrive after a newer change); the lifeline is a multi-subscription with an ID, repaired after a dropped update; the feeds view matches `shell/feeds/**`, including a topic first published while it is open, and not `shell/other/noise` | `TestShellFeedsGlob`, `TestShellBoardRecoversAfterLoss`, `TestShellBoardSnapshotPrecedesEveryChange`, `TestShellLifelineRecoversAfterDroppedUpdate`; `browser/tests/shell.spec.js` | Partial: plain and scoped glob variants and inner-close termination planned (pass 005, 008) |
| Groups | `DynamicGroup`, `DynamicGroupHandler`, `DefineGroup`, `GroupHandler` | Collaboration feed: a dynamic group per role. Shell ops view: a static group of two topics, active only once both values have arrived. Event names are the topic names. Group streams are not loss-aware, so both apps republish whole values after a drop | `TestCollabFeedIsADynamicGroup`, `TestShellOpsGroup`, `TestShellOpsRecoversAfterDroppedUpdate`; `browser/tests/collab.spec.js`, `browser/tests/shell.spec.js` | Browser |
| Dynamic topics | `AddTopic`, `RemoveTopic`, `AddTopicForScope`, `RemoveTopicForScope`, `SubscriberTopics`, `tavern-topics-changed` | Shell: Follow and Unfollow add and remove an allowlisted channel on the window's own lifeline; a lead adds the leads channel to every lead's lifeline by role | `TestShellFollowChangesLifelineTopics`, `TestShellLeadsChannelAndNotesFollowRole`, `TestShellLifelineBelongsToItsWindow`; `browser/tests/shell.spec.js` | Browser. An added topic is a plain subscription: it starts with the topic's cached latest message, if any, and does not take the multi-subscription's other options. `AddTopicForScope` finds multi-subscribers through another subscription with the same subscriber ID that has the scope; the shell's lifeline holds one |
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
| Backpressure and slow subscribers | `WithAdaptiveBackpressure`, `AdaptiveBackpressure`, `BackpressureTier`, `OnBackpressureTierChange`, `SetSimplifiedRenderer`, `tavern-backpressure`, `WithSlowSubscriberEviction`, `WithSlowSubscriberCallback`, `OnPublishDrop`, `PublishDrops` | Board: a slow window ends on overflow (loss-aware, not adaptive). Collaboration: a paused feed overflows and `OnPublishDrop` triggers a republish of the current feed | Capacity reports drops; `TestCollabFeedRecoversAfterDroppedUpdate`, `TestCollabFeedStaysStaleWithoutRepair`; `browser/tests/collab.spec.js` | Partial: adaptive tiers planned (pass 006). Recovery: `WithSlowSubscriberEviction(3)` ends a held-back window with reason `evicted`, which reconnects to a snapshot; drop-newest is repaired from `OnPublishDrop` (`TestRecoveryFullQueues`, `browser/tests/recovery.spec.js`) |
| Hooks and middleware | `After`, `OnMutate`, `NotifyMutate`, `MutationEvent`, `Use`, `UseTopics`, `Middleware`, `OnFirstSubscriber`, `OnLastUnsubscribe` | — | — | Planned (pass 006) |
| Scheduled publishing | `RunPublisher`, `NewScheduledPublisher`, `Register`, `SetInterval`, `Start`, `WithBaseTick`, `SectionOptions`, `CircuitBreakerConfig`, `OnRenderError`, `RenderError` | — | — | Planned (pass 006) |
| Introspection and metrics | `HasSubscribers`, `SubscriberCount`, `TopicCounts`, `Stats`, `WithMetrics`, `Metrics`, `WithObservability`, `Observability`, `WithConnectionEvents` | Go tests read `HasSubscribers` and `SubscriberCount` | Board tests; capacity | Partial: an operations view planned (pass 007) |
| Backends | `WithBackend`, `backend.Backend`, `HealthAwareBackend`, `ObservableBackend`, `backend/memory` | — | — | Planned (pass 007) |
| Presence | `presence.Tracker` (`Join`, `Leave`, `Update`, `Heartbeat`, `List`, `Get`) | — | — | Planned (pass 007) |
| Test helpers | `taverntest` (`Capture`, `Recorder`, `SSERecorder`, `MockBroker`, `SimulatedConnection`, `SlowSubscriber`) | — | — | Planned (pass 008) |
| Metrics adapters | `tavernprom`, `tavernotel` modules | — | Their own module tests | Planned (pass 007) |
| Proxy deployment | nginx configuration, heartbeats, shutdown through the proxy | `deploy/` | `make proxy-smoke` in CI | Exercise |
| Capacity | broker and HTTP load presets | `examples/capacity` | `make capacity` | Exercise |

## tavern-js 0.0.24

| Capability | Attributes, events and API | Scenario | Evidence | Status |
|---|---|---|---|---|
| Discovery | automatic binding of `sse-connect` elements on load | Connection | Browser tests | Browser |
| Dynamic binding and teardown | `MutationObserver` binding of inserted `sse-connect` elements; `Tavern.bind`, `init`, `scanAndBind`, `destroy` | Shell: each view is swapped in by htmx and bound once by tavern-js; leaving it retires its registry entry, and htmx's cleanup of the swapped-out element closes its connection | `browser/tests/shell.spec.js`: one lifeline request across repeated navigation, the server's board stream released when the view is left, subscriber counts back to where they started | Partial: the explicit `bind`, `init`, `scanAndBind` and `destroy` calls are not exercised. `retire` and `destroy` only change tavern-js's registry; closing a connection is htmx's job |
| Connection state | `tavern-live-class`, `tavern-reconnecting-class`, `tavern-status`, `tavern-status-live`, `tavern-status-recovering` | Connection | Browser tests assert the classes and the live and lost status elements; the recovering element is configured but shown too briefly to assert | Browser |
| Stale state | `tavern-stale-class`, `tavern-status-stale`, `tavern:stale` | Connection (configured; this scenario never goes stale) | — | Planned (pass 005) |
| Lifecycle events | `tavern:live`, `tavern:disconnected`, `tavern:recovering`, `tavern:reconnected` (with replay counts), `tavern:transport-closed`, `tavern:transport-open` | Connection event log | Browser reconnect test asserts their order | Browser |
| Region updates | `tavern:region-updated`, `tavern-updated-class`, `tavern-updated-ms` | Connection | Browser test asserts the event; the class is configured, not asserted | Partial |
| Gap handling | `tavern-gap-action` (`reload`, `banner`, custom event), `tavern-gap-banner-text`, `tavern:replay-gap`, `tavern:replay-truncated` | Recovery: the resume page with each gap action and with none. Every gap marks the region stale; the snapshot and confirmation that follow bring it back to live on the current value. Reload happens once: the new page starts from a snapshot. The custom handler's fetch is applied only when its revision is newer than the stream's | `browser/tests/recovery.spec.js` (gap actions, default action, delayed, overlapping and failing custom fetches) | Partial: `tavern-gap-banner-text` not set (default text shown); `tavern:replay-truncated` cannot arrive after a full queue (see Gaps and truncation) |
| Topic changes | `tavern:topics-changed` | Shell: the lifeline shows its current topics from each event's `topics` list | `browser/tests/shell.spec.js` | Browser |
| Degradation | `tavern:stream-degraded`, `tavern:stream-restored` | — | — | Planned (pass 006) |
| Commands | `tavern-hearth` (pointerdown with its click fallback), `tavern-command-delegate="click"` with `tavern-command-target`, `tavern-command-dedup`, `{id}` URL token expansion, `Tavern.command` (resync requests; its rejection is shown), `tavern:command-sent`, `-success`, `-error` with `detail.element` | Lab: Votes uses hearth with a 400 ms dedup window; Server log uses click delegation with a 1 s window. Command bodies are JSON, validated by the server. The page shows Sending, Accepted or Failed for each command, naming it by `detail.element` | `browser/tests/lab.spec.js`: one POST per click, Enter on a focused button, a double click sent once, a target replaced mid-gesture sent once; a refused command reported once from its connected button; a vote accepted after its button was replaced and a vote refused (404) after its task was deleted, each reported once from the region with the original element; `TestLabCommandsValidateJSON` | Browser. Outcomes come from the command element while it is connected, otherwise from the region, otherwise the document ([tavern-js#54](https://github.com/catgoose/tavern-js/issues/54), fixed in v0.0.24). Dedup is per element and URL in the browser; it is not server idempotency. |
| Interaction protection | `tavern-hot-policy` (`pause-on-pointerdown`, `defer-on-focus`, both), `tavern:policy-activated`, `tavern:policy-deactivated` (`flushed`) | Lab: Votes uses pause-on-pointerdown; Editor uses both. Held updates are discarded by tavern-js; when `flushed` > 0 the page asks for a snapshot on that region's own stream | `browser/tests/lab.spec.js`: release outside the region, a cancelled touch, focus moving between controls, pointer and focus together releasing only when both end, one update then silence converging, a second catch-up needed while the first request is still open; `TestLabResyncReachesOnlyItsOwnStream` | Browser. No library change was needed for [tavern-js#53](https://github.com/catgoose/tavern-js/issues/53); its recipe and tests remain open upstream |
| Live form editing | `defer-on-focus` plus an application draft guard on `htmx:oobBeforeSwap` | Lab Editor: an open editor keeps its value and selection through updates and snapshots, says when its task changed or was deleted, and saves only from the title revision it started with | `browser/tests/lab.spec.js`; `TestLabSavesFromOneRevisionConflict`, `TestLabSaveAfterDeleteKeepsDraftAndDoesNotRecreate` | Browser. Works with the pinned release; [tavern-js#50](https://github.com/catgoose/tavern-js/issues/50) remains open for its library deliverables |
| Lifeline and scoped streams | `tavern-role`, `tavern-scope`, `tavern:stream-warming`, `-ready`, `-promoted`, `-retired`, `-fallback`, `Tavern.lifeline`, `stream`, `streams`, `promote`, `retire` | Shell: one lifeline, which the views wait for; each view is a scoped stream, promoted only when every value it starts from has arrived since it was mounted or reconnected and it is still on screen; a broken view raises `tavern:stream-fallback` on the lifeline and is promoted again after its new snapshot | `browser/tests/shell.spec.js`: registry contents after each navigation, fallback while notices still arrive, recovery, an aborted view response, a view left while its stream was opening never promoted, an ops view missing one value not promoted, a slow lifeline with no early view requests | Browser. `tavern-scope` is a client registry name, not a broker scope or a permission |
| Debug logging | `tavern-debug` | — | — | Planned (pass 008) |

## Notes

- A fresh connection is marked live when its transport opens, before any
  server message. After a drop it becomes live only when the server sends
  `tavern-reconnected`. That event is sent only on a resume with
  `Last-Event-ID`, so tavern-js recovery status belongs on ID-backed
  streams. The board's invalidation stream has no event IDs and keeps its
  own ready/refetch status, so tavern-js is not loaded on the board.
- htmx logs `htmx:sseError` events with `console.error`, so the browser
  tests declare one such error for each stream they end.
- tavern-js marks a region live after a reconnect only when the server
  sends `tavern-reconnected`. A failed strict replay and a truncated replay
  send none, so those regions stay in *Reconnecting* even once the value is
  current; the recovery lab says so rather than sending a confirmation of
  its own.
- tavern-js holds updates back while a region is protected and discards
  them when protection ends, keeping no record of what they held. A region
  whose last update arrived while it was protected stays out of date until
  something else arrives, so the lab asks the server for a snapshot when
  `flushed` is above zero, and shows "Catching up" until it lands.
- htmx focuses `[autofocus]` elements in swapped content. The lab's draft
  guard removes `autofocus` from an editor it keeps, or a snapshot would
  pull focus back into the region and hold every later update.
