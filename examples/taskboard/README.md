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
resume replays what it missed before `tavern-reconnected`.

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

CI runs them on every pull request and on `main`
(`.github/workflows/validate.yml`, job *reference app in a browser*).

## Vendored browser files

| File | Version | Source | License |
|---|---|---|---|
| `htmx-2.0.11.min.js` | htmx 2.0.11 | npm registry, integrity verified | 0BSD (`LICENSE.htmx`) |
| `htmx-ext-sse-2.2.4.min.js` | htmx-ext-sse 2.2.4 | npm registry, integrity verified | 0BSD (`LICENSE.htmx-ext-sse`) |
| `tavern-0.0.23.min.js` | tavern-js v0.0.23 (tag object `82d3ad8`, commit `f3166d8e781b250cf604e0f5ed44c306284d33ff`) | `tavern.min.js` asset of the [v0.0.23 release](https://github.com/catgoose/tavern-js/releases/tag/v0.0.23) | MIT (`LICENSE.tavern-js`, from that commit) |

`vendor.sha256` records their digests. Scripts load in this order: htmx,
the SSE extension, then tavern-js, which binds to the extension's events.

```sh
./vendor.sh check      # files match vendor.sha256 (offline)
./vendor.sh upstream   # and tavern-js matches the pinned release (make verify-assets)
./vendor.sh update     # download the pinned tavern-js release
```
