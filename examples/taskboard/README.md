# Task board reference

A shared task board built with Go `net/http`, HTMX and Tavern. Every window
keeps one SSE connection; changes made in one window appear in the others,
and a window that falls behind recovers by reloading the board.

```sh
go run ./examples/taskboard            # http://127.0.0.1:8080
go run ./examples/taskboard -h         # buffer, heartbeat, write timeout, admission
```

Requirements: Go 1.27.3 or later. The browser files are vendored in
`static/` with their licenses, so there is no frontend build, Node or CDN
(see [Vendored browser files](#vendored-browser-files)).

## Try it

1. Open the page in two windows and add or move tasks in either one.
2. Open **Try a slow connection** in one window, press **Pause updates**, then
   **Add 10 sample tasks** (more changes than that window may queue), then
   **Resume**. The window shows *Reconnecting…*, then *Live*, and the board
   shows every task.
3. Stop the server with Ctrl-C: windows show *Reconnecting…* and reconnect
   when it is back. The in-memory board starts empty again.

## Showcase

**Showcase** in the board's header lists working scenarios for Tavern and
[tavern-js](https://github.com/catgoose/tavern-js). [COVERAGE.md](COVERAGE.md)
lists every capability of both libraries, what demonstrates it, and what is
still planned.

**Connection and live updates** (`/showcase/connection`) is a counter
published with event IDs (`PublishWithID`, kept by `SetReplayPolicy`) and
streamed with `SubscribeFromIDWith` and `StreamSSE`. tavern-js shows the
region's state from real connection events:

1. Open it in two windows. Both show *Live*; **Increment** updates both.
2. **Drop, then increment while away** ends this window's stream on the
   server and changes the counter before the window can reconnect. The
   region shows *Connection lost*. After the browser's reconnect delay it
   reconnects with `Last-Event-ID`, the server replays the missed value and
   sends `tavern-reconnected`, and the region shows *Live* again. The event
   log shows `replayDelivered: 1`.

A fresh connection replays the values the broker still keeps (four),
ending with the current one, so a change made while the page loads is not
missed.

**Collaboration** (`/showcase/collaboration`) is a second board shared by
two demo people: Alice, a team lead, and Bob, a contributor. Open it as
Alice in one window and as Bob in another.

1. Add, rename (**Edit**), move, assign and delete tasks in either window;
   both boards follow. Titles are shown as text, whatever they contain.
2. Alice can add an **Internal** task. Bob's board, counts and activity
   never mention it, and his requests for it get 404.
3. **Assign Bob** puts a notice in Bob's inbox only. Moving a task to Done
   tells its creator, but only Alice takes those notices; Bob's stream
   filters them out.
4. **Clear finished tasks** removes every finished task the person can see.
5. **Try a reconnect** → **Disconnect this window** ends the window's
   board stream and holds its reconnect for up to a minute. Change things in
   the other window, then **Reconnect**: the board and inbox are rebuilt
   from the server. A hold that is never resumed expires and the window
   reconnects by itself; at most 32 windows can be held at once, and
   shutdown releases them all.
6. **Try a slow feed** → **Pause this window's feed** holds back the
   window's activity and counts on the server. Add tasks in the other window
   until its feed queue overflows, stop, then **Resume the feed**: within a
   couple of seconds the counts and activity are current again, with no
   further change to the board.

The person selector is a demo switch, not a login. Each request names a
person; what that person may see comes from a fixed definition on the
server (`collab.go`), never from topics or scopes in the request. A real
application takes identity from its session.

How it is delivered:

- **One stream per window for the board and inbox**: `SubscribeWith` with
  `SubWithScope(person)`, `SubWithSnapshot`, `SubWithFilter`,
  `SubWithDisconnectOnLoss` and `SubWithStatus`, written by `StreamSSE`.
  Changes are published to each person allowed to see them with
  `PublishLazyOOBTo`, which renders only while someone watches the board.
  They are real OOB fragments: `AppendComponent` for a new card,
  `ReplaceComponent` for an edit, `Delete` then `AppendComponent` for a
  move, `Delete` for a removal, and `Prepend` for a notice (with `Delete`
  for the oldest beyond eight). Cards implement `tavern.Component`.
- **Snapshot, then live.** The snapshot replaces every column and the inbox.
  It is taken, and the subscription registered, while the board's lock is
  held; every change takes the same lock and publishes before releasing it.
  So each change is either in the snapshot or delivered after it. The board
  topic has no replay cache, so nothing older follows the snapshot. A
  reconnect, or a stream ended because it fell behind, starts over from a
  new snapshot.
- **The inbox filter.** Notices are SSE frames whose event name carries the
  kind (`inbox-assigned`, `inbox-completed`). The predicate reads only that
  first line. Snapshots apply the same rule, so excluded notices never reach
  the browser.
- **A topic group for the feed**: `DynamicGroup` chooses the role's
  activity and counts topics, and `DynamicGroupHandler` serves both on a
  second stream, with the topic names as event names. Both are published
  whole with `PublishWithReplay`, so a new feed stream starts from the latest
  of each.
- **Repairing the feed after loss.** The group handler's stream does not
  end when its queue is full; the update is dropped and the stream stays
  open. Because each feed update is a whole representation, the app
  repairs this itself: the broker's `OnPublishDrop` callback schedules a
  republish of the current feed 200 ms later. A republish that is dropped
  again schedules another, waiting twice as long up to 2 s, and they stop
  once one goes out without a drop. Updates already queued are delivered
  first, so the newest state arrives last. While a stream stays stuck, the
  republish repeats at most every 2 s. A client that stops reading is ended
  by the write timeout; a feed paused by the demo stays open until it is
  resumed or the window closes.
- **Clear finished tasks** uses `Batch`: each person's removals are flushed
  as one event. Batching joins messages for the same topic and scope into a
  single write. It is not a transaction: the board and the feed are separate
  publishes, and the browser applies each event when it arrives.

The page therefore holds two event streams, one for the board and one for
the feed.

**Interaction lab** (`/showcase/lab`) uses tavern-js to keep clicks and
typing safe while a list keeps changing. Open it as Alice and as Bob. The
same tasks appear in three regions, each on its own stream:

- **Votes** (`tavern-hearth`, `tavern-command-dedup="400"`,
  `tavern-hot-policy="pause-on-pointerdown"`). **Vote** is a tavern-js
  command sent on pointerdown, with `{id}` in its URL filled from
  `command-id`. A click, Enter on the focused button, or a quick double
  click each send one command. While the pointer is down, updates are held;
  releasing it anywhere ends the hold.
- **Editor** (`pause-on-pointerdown defer-on-focus`). **Edit** opens a form
  in the list. While anything in the region has focus or the pointer is
  down, updates are held; they resume only when both end. An open editor
  keeps what you typed, and where the cursor is, through every update: an
  application guard on `htmx:oobBeforeSwap` leaves it in place and says when
  the task changed or was deleted on the server. Saving sends the title
  revision the editor started from; the server compares it under its lock,
  so of two saves from one revision only the first wins. The other gets a
  409 showing the current title, keeps the draft, and offers **Save mine
  over theirs** or **Keep theirs**. Saving a deleted task keeps the draft and
  does not bring the task back.
- **Server log** (`tavern-command-delegate="click"`,
  `tavern-command-dedup="1000"`). Each person sees only entries about tasks
  they may see, live and in snapshots. **Simulate 8 votes** starts a finite
  burst of updates (one at a time, 150 ms apart); a second request while one
  runs is refused with 409, and the page shows the failure.

**Catching up after held updates.** tavern-js discards the updates it held
back. When its `tavern:policy-deactivated` event reports any (`flushed` >
0), the page calls `Tavern.command` to ask for a snapshot. The server sends
it on that region's own stream, which is scoped to the window, so it
arrives after every earlier change and before any later one. The region
shows *Paused*, then *Catching up*, then *Up to date*, and never claims to be
current while updates are held. This needs no further updates from anyone.
One request runs at a time; if updates are held and discarded again while
it is open, another snapshot is requested when it settles, unless a newer
snapshot has already arrived or the region is protected again.

Each command's status line goes from *Sending* to *Accepted* (the server
took it; the regions then update from their streams) or *Failed* with the
reason. tavern-js delivers the outcome even when the live update has already
replaced the button: from the button while it is connected, otherwise from
the region, with `detail.element` naming the button.

Command bodies are JSON (`command-*` attributes as strings). The server
accepts only `application/json` (415 otherwise). The whole body must fit in
1 KB (413 otherwise) and be exactly one object with no unknown fields,
optionally surrounded by whitespace; anything after it is refused (400).
The task ID must match the URL. Browser dedup is per element and
URL; it is not server-side idempotency.


**App shell** (`/showcase/shell`) keeps one connection, the *lifeline*,
open for the whole page and swaps views in and out below it, each with its
own stream. Open it as Alice, and as Bob in a second window.

1. Switch between **Board**, **Feeds** and **Ops**. The notices at the top
   keep arriving; the lifeline never reconnects. **Show server state**
   lists what the server holds for this window.
2. **Follow** a channel: the server adds its topic to this window's
   lifeline (`AddTopic`), which receives the channel's latest post at once
   and every later one, and lists the topic. **Unfollow** removes it. As
   Alice, **Add for every lead** adds the leads channel to every lead's
   lifeline (`AddTopicForScope`); Bob's never gets it.
3. In **Feeds**, publish to the three topics: only the two under
   `shell/feeds/` appear, each labelled with its topic.
4. In **Board**, **Break this view's stream**: the lifeline reports the
   fallback, notices still arrive, and the board waits. **Let it
   reconnect**: the view starts again from a new snapshot, including changes
   made meanwhile.
5. **Hold back the lifeline and ops view**, post notices and record deploys
   in the other window until the queues overflow, then **Release them**:
   within a couple of seconds both show the current values, with nothing
   more posted. **Open … with a slow lifeline** on the chooser shows the
   views waiting for the lifeline to start.

How it is built:

- **Lifeline** (`tavern-role="lifeline"`):
  - `SubscribeMultiWithMeta` with the window's connection ID as subscriber
    ID, on the notices topic. Channels are added and removed only from a
    fixed allowlist, and only for a connection owned by the requesting
    person.
  - A second subscription, scoped to the person's role and with the same
    subscriber ID, carries role notes. Its scope is also what
    `AddTopicForScope` matches: a multi-subscription has no scope of its
    own.
  - Topics added this way are plain subscriptions. They start with the
    topic's latest cached message, if there is one, but do not inherit
    other options.
  - The page shows the topic list from each `tavern:topics-changed` event.
  - Notices, channel posts and role notes are whole representations. If
    one is dropped from a full queue, the server republishes the current
    ones (the collaboration feed's repair, 200 ms then backing off to 2 s),
    so the lifeline catches up without reconnecting or losing the topics it
    follows.
  - Views wait for the lifeline: the navigation stays disabled, and the first
    view is requested only once the lifeline's first message shows that the
    server has registered it.
- **Views** (`tavern-role="scoped"`):
  - Each view element is swapped in by htmx and bound by tavern-js, which
    registers it under its `tavern-scope` name as *warming*, then *ready*
    when its connection opens.
  - The page promotes it to *active* only when every value the view starts
    from (`data-ready-events`) has arrived since it was mounted or last
    reconnected, and it is still the view on screen. An open connection is
    not a loaded view.
  - Leaving a view retires its entry. Retiring only changes tavern-js's
    registry: the connection closes because htmx cleans up the element it
    swaps out, and the server's handler then unsubscribes.
  - Navigation requests replace each other (`hx-sync`), so a slow earlier
    response never lands.
- **Board view**: `SubscribeMultiWith` over two topics with
  `SubWithDisconnectOnLoss` and `SubWithStatus`, checked once with
  `ValidateSubscribeOptions`. The snapshot is taken and the subscription
  registered under the shell's lock, and the snapshot is written once,
  before any message, with `WithStreamSnapshot`. (`SubWithSnapshot` on a
  multi-topic subscription queues one copy per topic, and the merged stream
  can deliver the second copy after a newer change.) A stream that falls
  behind ends, and the next one starts from a new snapshot.
- **Feeds view**: `SubscribeGlobWith("shell/feeds/**")` with a ready frame
  from `WithStreamSnapshot`. Glob subscriptions cannot take a snapshot or be
  loss-aware, so this view is live only and best effort: it shows what
  arrives while it is open, keeps the newest eight, and an item dropped from
  a full queue is not sent again.
- **Ops view**: a static group (`DefineGroup`, `GroupHandler`) of two
  topics whose latest values are kept with `PublishWithReplay`. Opening the
  view starts from both, and it is active only once both have arrived. The
  group stream is not loss-aware, so a dropped value is repaired like the
  lifeline's.
- **Breaking the board stream** reuses the collaboration scenario's bounded
  holds. tavern-js raises `tavern:stream-fallback` on the lifeline for an
  *active* scoped stream that errors. The lifeline does not carry the
  board's data; the view recovers by reconnecting and taking a new snapshot.

`tavern-scope` names a stream in tavern-js's registry. It is not a broker
scope (`SubWithScope`), and neither grants access. Every request names a
person. Requests about a window's connection (the lifeline itself, view
streams, following channels, holding back, breaking and recovering the
board, and the server state) are refused unless that connection's lifeline
is open and belongs to the person. Shared actions (posting notices and
channel posts, role notes, board tasks, feed items and ops events) take
only the person. `ScopedTopic` only builds
colon-separated topic names; glob patterns match slash-separated paths.

**Recovery lab** (`/showcase/recovery`) shows what happens to a window
that misses updates, and how it gets back to the current value. Each
scenario is a page with one value shared by every window; the buttons that
cause a loss act on your window's connection only. **Show what the server
saw** lists the server's observations (reconnects, gaps, store failures,
streams that ended), and the page logs each tavern-js event it sees.

1. **Resume with Last-Event-ID** (`resume`): **Disconnect and miss 3
   changes** ends this window's stream, holds its reconnect while three
   changes are made, then lets the browser reconnect with the last ID it
   saw. The server replays the three and confirms with `tavern-reconnected
   {"replayDelivered":3,"replayDropped":0}`. **Miss 8** is longer than the
   five values kept, so it is a gap: `tavern-replay-gap`, then the current
   value as a snapshot, then the confirmation. The index links the same
   page with each gap action: **banner** shows tavern-js's banner,
   **reload** reloads the page once (the new page starts from a snapshot,
   so it does not reload again), and **custom event** has the page fetch
   the current value. The fetch is applied only if its revision (the event
   ID the values were published with) is newer than the last one the
   stream delivered, so a slow response cannot replace a newer update. A
   newer gap cancels an older fetch, and a failing one is tried three times
   and then left to the stream.
2. **Gap without a snapshot** (`resume-silent`) is the same with
   `GapSilent`: after a long absence the window goes live with an old value
   and stays behind until the next change. Only the server sees the gap.
   Silent continuation is not reconciliation.
3. **Two topics on one resume** (`multi`): `SubscribeMultiFromID` resumes
   two topics from one `Last-Event-ID`. Each change publishes both values
   with the same ID, so the ID is in both logs; each topic replays, reports
   gaps and confirms on its own, and there is no order between them. One
   topic's replay is bundled into a single write (`SetBundleOnReconnect`).
4. **Replay larger than the queue** (`truncation`): queues of four, sixteen
   values kept. **Miss 10** replays four and drops six; the reconnected
   and truncated notices queue after the replay and are dropped too, so
   tavern-js never gets a confirmation and the region stays in
   *Reconnecting*. The server sees the counts in `OnReconnect` and
   republishes the current value until it is delivered. **Reconnect now**
   resumes with nothing missed, which the server confirms, and the region
   goes live.
5. **Full queues** (`drop-newest`, `drop-oldest`, `evict`, `loss-aware`):
   **Hold back this window**, **Make 10 changes** twice, **Release it**.
   Each is its own broker with queues of four. Dropping the newest loses
   the final value, so the server republishes it after a drop
   (`OnPublishDrop`); `WithDropOldest` keeps the newest without help;
   `WithSlowSubscriberEviction(3)` closes the subscription after three
   drops in a row; `SubWithDisconnectOnLoss` ends it at the first drop.
   The last two record the first termination reason (`evicted`,
   `queue-loss`) and the browser reconnects to a snapshot.
6. **Replay store failures** (`store`, `store-strict`): replay kept in a
   `ReplayStore`, an in-memory one wrapped so that appends and reads can be
   made to fail (the store is shared by both pages and every window).
   Failures reach `WithReplayStoreErrorHandler` as a `ReplayStoreError`
   with its operation and cause. By default a failed read is treated as a
   gap: the window gets the gap event and a snapshot. Failed appends are
   lost without a gap: the window's ID is still stored and nothing after it
   was kept, so the resume looks complete and only the handler saw it.
   That window recovers, with nothing more changed, once its history is
   gone: **Repair the store**, **Clear replay history**, then **Reconnect
   now**. The resume no longer finds its ID, so it is a gap answered with a
   snapshot.
   With `SubWithTerminateOnReplayError` a failed read ends the subscription
   (`replay-failure`) instead; the app then serves a `SubscribeWithSnapshot`
   subscription on the same response, so the window still reaches the
   current value, but no reconnected notice is sent, so it stays in
   *Reconnecting* until a resume succeeds: **Repair the store**, then
   **Reconnect now**. **Clear replay history** calls `ClearReplay`, which
   also removes the topic's replay, gap and bundling policies; the app sets
   them again.

How it is built:

- Every scenario runs on its own broker, created in `newRecovery`, so
  buffer size, drop policy, eviction and the replay store never touch the
  app broker or its publish-drop callback. All are closed at shutdown.
- A fresh window starts from a snapshot carrying the latest event ID, and
  no replay. A window resuming with `Last-Event-ID` gets
  `SubscribeFromIDWith` with its connection ID as subscriber ID, so the
  server's reconnect and gap observations name it.
- Each scenario holds one publication lock across every publish (including
  repair) and across each subscribe, with its replay and gap snapshot. A
  change made while a snapshot is taken is queued after it, so a snapshot
  never replaces a newer value or moves the window's event ID back. The gap
  snapshot callback, which the broker calls during the subscribe, takes
  only the model lock.
- A connection ID has one open stream at a time. It is reserved before the
  stream's pause gate is published or its subscription is made; a second
  stream with the same ID is refused (409) and the first keeps its
  controls. The reservation is released when the stream ends, before a
  held reconnect is let through. At most 32 streams are open per scenario.
- **Disconnect** ends the window's own stream and holds its reconnect
  (the collaboration scenario's bounded holds) while the changes are made,
  so the absence is exactly that many changes. Holds end on release,
  expiry, cancel and shutdown.
- **Hold back** pauses that window's writes (the shell's pause gate) until
  its queue overflows.
- Callbacks only record or schedule: `OnReconnect` and `OnReplayGap` run in
  their own goroutines, and the store error handler, which runs
  synchronously during a publish, only appends to a bounded log that has
  its own lock.
**Delivery lab** (`/showcase/delivery`) shows how Tavern shapes what it
sends. Each page runs on its own broker, so its options and callbacks do
not reach the rest of the app. Values and settings are shared by every
window; **Hold back** and the slow panels act on one window only. **Show
server state** lists the server's counters and what it saw.

1. **Change-only and lazy rendering** (`render`): four fields published
   only when their content changes (`PublishIfChangedOOB`,
   `PublishIfChanged`, and for the red team `PublishIfChangedOOBTo` and
   `PublishIfChangedTo`); setting the same value again is skipped, and
   **Forget the last status** (`ClearDedup`) lets it through once.
   **Add activity** prepends a server-rendered component
   (`PrependComponent`, `PublishOOB`) and deletes the oldest beyond eight; a
   component that fails to render is caught with `RenderComponentErr` and
   not published. The report is rendered only while some window has its
   panel open (`PublishLazyOOB`, `PublishLazyOOBTo` and the `IfChanged`
   forms); the counters show refreshes asked, rendered and published.
2. **Immediate, debounced and throttled** (`timing`): one burst of twenty
   changes published at once, with `PublishDebounced` (300 ms of quiet) and
   with `PublishThrottled` (the first at once, then the latest at most every
   250 ms). All three end on the same value; the counts differ. Every change
   goes through the same calls, so a delayed publish never carries an older
   value than one already sent on its topic.
3. **Expiring messages** (`ttl`): messages with a 3 s time-to-live and a
   100 ms sweep (`WithMessageTTLSweep`). A notice (`PublishWithTTL` with
   `WithAutoRemove`) disappears from open windows when it expires; the
   banner (`PublishOOBWithTTL`) and the team notice (`PublishToWithTTL`)
   stay on pages that show them. A window opened before expiry gets them
   from the replay cache, one opened after gets nothing. At most five notices
   show at once: the replay cache keeps five, and expiry removes only
   entries still in it, so a notice pushed out by a sixth would never be
   removed. A sixth is refused until one expires; the app learns that a
   notice has gone by seeing the sweeper's removal publish. The status uses
   `PublishIfChangedWithTTL`. Each window also subscribes to a topic of its
   own, which the broker drops about a second after the window closes
   (`WithTopicTTL`). The replay cache is per topic, not per scope, so the
   team notice uses a topic that only the red team subscribes to (#245).
4. **Batches and ordering** (`batch`): a batch of two board updates and a
   feed item for everyone and one of each for the red team. `Flush` sends one
   channel message per topic and scope, so the window's four subscriptions
   get one each; `Discard` sends nothing. `PublishWithID` and
   `PublishWithTTL` on a batch are not buffered: they are delivered before
   the batch's buffered update. **Run
   concurrent producers** has four producers publish 25 messages each to an
   ordered topic (`SetOrdered`) and a plain one; the two subscribers of the
   ordered topic see the same order. Each producer's messages stay in its
   order on both; there is no order across topics.
5. **Rate limits and coalescing** (`shaping`): fifty values in a quarter of
   a second, to three slow panels (each write waits 60 ms). The plain panel
   drops what does not fit and can miss the final value; the rate-limited
   one (`SubWithRate`) gets the latest at most every 200 ms; the coalescing
   one (`SubscribeWithCoalescing`) gets the latest whenever it is ready.
   Replaced values are not drops. **The named forms** opens
   `SubscribeWithRate` (per second), `SubscribeScopedWithRate` and
   `SubscribeScopedWithCoalescing` panels; **metadata and filters** opens
   `SubscribeWithMeta`, `SubscribeWithFilter` and
   `SubscribeScopedWithFilter` panels, the filters passing every tenth
   value.
6. **Adaptive backpressure** (`pressure`): queues of four with
   `WithAdaptiveBackpressure` (throttle after 2 consecutive drops, simplify
   after 4, evict after 8; a successful send resets). Hold the window back,
   make changes, release it: `OnBackpressureTierChange` lists the
   transitions, simplified content (`SetSimplifiedRenderer`) still carries
   the value, and the current value is republished after a drop until it is
   delivered. Thirty changes evict the window, which reconnects to a
   snapshot. Tier hints (`tavern-backpressure`, reported by tavern-js as
   `tavern:stream-degraded` and `tavern:stream-restored`) are sent without
   waiting, so one that finds the queue full is dropped; the server's list
   has every change. **Publish a change and wait up to 300 ms** uses
   `PublishBlocking` (`PublishBlockingTo` for the team) and shows
   `ErrPublishTimeout` while the window is held back.
7. **Reactive workflow** (`flow`): adding an order changes the model, then
   calls `NotifyMutate`; the `OnMutate` handler publishes the row and an
   audit entry, and an `After` hook on the list publishes the derived
   summary as a separate step. `UseTopics("orders:*", …)` marks order rows
   and summaries as checked and holds back test orders; the `audit:log`
   topic does not match, so its entries stay unchecked. `Use` counts every
   publish. `OnFirstSubscriber` and `OnLastUnsubscribe` start and stop a
   worker that refreshes the summary's clock. Six orders and eight audit
   entries are kept, and older ones are removed from the page. Each
   mutation and its publishes, each summary, and each window's snapshot and
   subscription take one publication lock, so a later order cannot be
   published before an earlier one, a summary always shows the model as it
   is when published, and a window opened or reloaded later starts from the
   current rows, summary and audit log (passed through the same check as
   the middleware) written once before any live update.
8. **Scheduled sections** (`sched`): a `ScheduledPublisher`
   (`WithBaseTick(50 ms)`, run with `RunPublisher`) renders a clock every
   second and a load reading every 400 ms, only while some window is
   subscribed. **Clock every 300 ms** calls `SetInterval`. Make the load
   source fail: each failure reaches `OnRenderError` as a `RenderError`;
   after two in a row the section's circuit (`CircuitBreakerConfig`) opens
   and the fallback is shown; after 1.5 s it tries again and, once the
   source works, returns to live readings.

How it is built:

- Every family's streams reserve the window's connection ID first (the
  recovery lab's registry, now shared): a second stream with the same ID is
  refused (409), and at most 32 streams are open per family. Pause gates,
  subscriptions and reservations are released when a stream ends.
- A window's subscriptions are merged into one response; if any of them
  ends (an eviction, for example) the whole stream ends and the browser
  reconnects.
- Where a page starts from a snapshot, it is taken with the subscriptions
  under the family's publication lock, which every publish also takes.
- Bursts, ordering runs and the workflow worker are single, bounded
  background tasks, stopped at shutdown before the brokers close.
  Synchronous callbacks (`OnRenderError`, `OnPublishDrop`, middleware, the
  mutation handler) only record, publish or schedule; none waits or
  relocks a held model lock.

## How it works

- **Canonical state lives on the server** (`board.go`). Columns and the
  activity list are HTML fragments fetched with `hx-get`.
- **Messages are invalidations, not data.** A change publishes an SSE event
  named after each region it affected (`column-todo`, `column-doing`,
  `column-done`, `activity`), all on one topic. Each region refetches itself
  when its event arrives (`hx-trigger="sse:column-todo"`).
- **Subscribe, then fetch.** The stream's first frame is `ready`, written
  after the subscription exists. Regions load on `ready`, so a change can
  never fall between loading the board and subscribing. A change during a
  fetch triggers another fetch: htmx queues the last request while one is in
  flight.
- **Loss ends the stream.** The subscription uses `SubWithDisconnectOnLoss`.
  If a window cannot keep up, its stream ends instead of skipping a change;
  the browser reconnects, gets `ready` again and reloads every region. The
  server logs each stream's end reason from `SubWithStatus` (`queue-loss`,
  `unsubscribe`, `broker-closed`).
- **Operations settings** follow the
  [production guide](../../docs/production-operations.md): per-frame write
  timeout, heartbeats, an admission limit (503 when full), and a shutdown
  that sends a `retry:` hint, ends every stream, then stops the server.

The **Pause updates** control holds back that window's writes on the server,
so its queue really fills; it exists only for the demonstration. The name
field is display text, not authentication: a real application derives
identity and topic access from its session.

The board does not use ID-backed replay; the showcase's connection scenario
does. Not covered: scoped topics, persistence, multiple server instances.

## Behind a proxy

`deploy/nginx.conf` is a tested nginx configuration: buffering and
compression off for `/events`, HTTP/1.1 to the app, and a short
`proxy_read_timeout` that the heartbeat keeps from firing.

```sh
make proxy-smoke   # Docker and Linux host networking required
```

`deploy/smoke.sh` builds the app, runs it on 127.0.0.1:18081 behind
`nginx:1.30.6-alpine` (pinned by digest) on 127.0.0.1:18080 (override with
`SMOKE_APP_PORT` / `SMOKE_PROXY_PORT`), and runs
`TestThroughProxy` against the proxy. The test checks that the first frame
arrives while the connection stays open, a change reaches the stream, the
stream survives 4.5s of idle time against a 3s proxy read timeout, a paused
window's stream ends on overflow and a reconnect reloads the full board, and
SIGTERM delivers the `retry:` hint and ends the stream through the proxy.
It refuses to start if either port is already in use. Cleanup removes only
the container and process this run created, tracked by container ID and
PID, even when building or starting fails. `deploy/smoke_test.sh` checks
those failure paths with stub `docker`, `go` and `curl` commands. CI runs
both on every pull request.

## Tests

`taskboard_test.go` runs the app on a real HTTP server: page and assets,
two viewers receiving every change, overflow and reconnect, a client that
never reads, the admission limit, and shutdown. `showcase_test.go` checks the
counter stream: every viewer receives each value, and a dropped window's
resume replays what it missed before `tavern-reconnected`. `collab_test.go` reads the
collaboration streams: snapshots and live fragments per person, the inbox
scope and filter, the feed group, one batched event for the bulk action, a
change made while a snapshot is being taken, reconnect after changes, and
malformed input. `shell_test.go` checks lifeline ownership, topic changes by window and by
role, the glob and group streams, one board snapshot before every change,
board recovery after loss, lifeline and ops recovery after a dropped final
update (and staleness with repair off), the break hold, and release on
cancel and shutdown. `lab_test.go` checks the lab's JSON commands, races two saves from one
revision, saves a deleted task, scopes resync to its window, and bounds
bursts. `collab_recovery_test.go` drops a feed's final updates
and checks that it still converges with no further change, and that it does
not when repair is off; and covers reconnect holds that expire, outlive an
abandoned attempt, hit the limit, end at shutdown, and are released by
concurrent callers without disturbing a newer hold. `recovery_test.go`
reads the recovery streams: a short resume, a gap with a snapshot and a
silent gap, two topics on one resume, a truncated replay repaired (and
staying behind with repair off) and then confirmed, full queues with each
drop policy (with a no-repair control), eviction and loss termination, store
read and append failures with and without strict mode and recovery once
the fault clears (the window behind failed appends included, with nothing
more changed), clearing replay, a change published while a gap snapshot is
taken on one topic, two topics and the store, one owner per connection ID,
release of connections however they end, controls that act on one window,
and release of held windows at shutdown. `delivery_test.go` checks each
delivery family over HTTP: change-only fields and dedup reset with scoped
isolation, lazy rendering with and without an audience, components and a
refused render, the burst's final values and counts, TTL delivery, replay
expiry and auto-removal while a window stays connected, more notices than
the replay cache keeps refused until one expires, idle topic cleanup,
one channel message per topic and scope (read from a raw channel) and
discard, the ordered topic's single order under concurrent producers,
rate-limited and coalescing windows ending on the latest value while a
plain one falls behind, adaptive tiers, blocking-publish timeouts,
eviction and recovery, the mutation-to-summary chain with middleware
filtering and a non-matching topic, windows opened after orders starting
from the model, an order held in middleware while later ones wait, the
bounded audit log, worker start and stop, scheduled
sections with the circuit's fallback and recovery, one owner per
connection ID, and shutdown.

## Browser tests

`browser/` runs the app in headless Chromium with Playwright. It needs Node
20 or later; the app itself does not.

```sh
make browser-install   # npm ci and the matching Chromium build
make browser-test
TASKBOARD_BROWSER_CHANNEL=chrome make browser-test   # use installed Google Chrome
```

The tests build the app, start it on a free local port, and stop it
afterwards. Each window gets its own browser context. Every page fails its
test on a page error, a console error, a failed request, a 4xx or 5xx
response, or a request to any other host; a test that causes an error on
purpose declares it. The tests:

- load the board, add a task and follow the links to the showcase;
- check that tavern-js marks two windows live and that **Increment**
  updates both;
- drop one window and check the lost state, the replayed value,
  `replayDelivered: 1`, the order of tavern-js events, and the return to
  *Live*, while the other window stays connected.
- in the collaboration scenario, as Alice and Bob in separate contexts:
  shared create, rename, move, assign and delete; escaped titles; internal
  tasks and notices reaching only their person; group counts and activity;
  clearing finished tasks; a new viewer matching an existing one; a
  window reconnecting after deletes, renames and additions without
  duplicate, missing or deleted cards; and a paused feed that dropped
  updates catching up after the board goes quiet;
- in the app shell: one lifeline request and the same subscriber counts
  after switching views back and forth; topics added and removed on the
  lifeline, by window and by role; glob and group views; a broken view
  falling back and recovering while notices arrive; an aborted slow view
  response; a view left while its stream was opening never promoted; a
  slow lifeline with no early view requests; an ops view missing one value
  not promoted; a held-back lifeline and ops view catching up once the
  producer is quiet;
- in the interaction lab: one vote per click, Enter and double click; a
  vote whose button is replaced mid-gesture sent once; a refused command
  reported; pointer release outside the region and a cancelled touch; an
  update held during a press converging after release with nothing more
  sent; a vote accepted after its button was replaced and a vote refused
  after its task was deleted, each reported once; an editor keeping its text and selection while focus moves inside
  the region and until both protections end; a stale save refused with the
  draft kept; a deleted task's save refused without recreating it; and
  internal tasks hidden from Bob;
- in the recovery lab: a short disconnect resumed and confirmed; an absence
  beyond retention answered with a gap and a snapshot; a silent gap live
  with an old value until the next change; the banner, custom event and
  reload gap actions each ending on the current value, with one reload
  only; two topics resumed from one ID; a truncated replay repaired but not
  confirmed until a real resume; full queues with each drop policy,
  eviction and loss termination ending on the current value; replay
  store read failures in default and strict mode, recovering once the
  store is repaired; a window left behind by failed appends recovering
  with nothing more changed; and custom gap fetches that return after a
  newer update, overlap, or keep failing, none of which replace the
  newest value;
- in the delivery lab: each page reached from the showcase; a burst ending
  on one value at once, debounced and throttled, with their counts; an
  expiring notice leaving open windows while a banner stays, and windows
  opened later getting nothing; an order reaching the page through the
  mutation hook, middleware and derived summary, and a test order held
  back; slow rate-limited and coalescing panels ending on the latest value
  while the plain one falls behind; a held-back window throttled and
  recovering, and one evicted and reconnecting to the current value; a
  failing scheduled section falling back and recovering; change-only fields
  and a report rendered only while its panel is open; a batch arriving as
  one message per topic and scope; at most five notices showing, all
  leaving when they expire; and a second workflow window, and a reloaded
  one, showing the model's rows, summary and audit log.

CI runs them on every pull request and on `main`
(`.github/workflows/validate.yml`, job *reference app in a browser*).

## Vendored browser files

| File | Version | Source | License |
|---|---|---|---|
| `htmx-2.0.11.min.js` | htmx 2.0.11 | npm registry, integrity verified | 0BSD (`LICENSE.htmx`) |
| `htmx-ext-sse-2.2.4.min.js` | htmx-ext-sse 2.2.4 | npm registry, integrity verified | 0BSD (`LICENSE.htmx-ext-sse`) |
| `tavern-0.0.24.min.js` | tavern-js v0.0.24 (tag object `eaa3433`, commit `066f573f584ecd6eff0b576552dd2ea573d01bee`) | `tavern.min.js` asset of the [v0.0.24 release](https://github.com/catgoose/tavern-js/releases/tag/v0.0.24); byte-identical to `dist/tavern.min.js` at the reviewed merge `c765585`, so its header comment still says 0.0.23 | MIT (`LICENSE.tavern-js`, from that commit) |

`vendor.sha256` records their digests. Scripts load in this order: htmx,
the SSE extension, then tavern-js, which binds to the extension's events.

```sh
./vendor.sh check      # files match vendor.sha256 (offline)
./vendor.sh upstream   # and tavern-js matches the pinned release (make verify-assets)
./vendor.sh update     # download the pinned tavern-js release
```
