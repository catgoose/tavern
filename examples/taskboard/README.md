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
concurrent callers without disturbing a newer hold.

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
  internal tasks hidden from Bob.

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
