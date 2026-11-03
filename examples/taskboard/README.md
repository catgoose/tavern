# Task board reference

A shared task board built with Go `net/http`, HTMX and Tavern. Every window
keeps one SSE connection; changes made in one window appear in the others,
and a window that falls behind recovers by reloading the board.

```sh
go run ./examples/taskboard            # http://127.0.0.1:8080
go run ./examples/taskboard -h         # buffer, heartbeat, write timeout, admission
```

Requirements: Go 1.27.3 or later. HTMX 2.0.11 and htmx-ext-sse 2.2.4 are
vendored in `static/` with their 0BSD licenses (from the npm registry,
integrity verified), so there is no frontend build or CDN.

## Try it

1. Open the page in two windows and add or move tasks in either one.
2. Open **Try a slow connection** in one window, press **Pause updates**, then
   **Add 10 sample tasks** (more changes than that window may queue), then
   **Resume**. The window shows *Reconnecting…*, then *Live*, and the board
   shows every task.
3. Stop the server with Ctrl-C: windows show *Reconnecting…* and reconnect
   when it is back. The in-memory board starts empty again.

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

Not covered here: ID-backed replay (`Last-Event-ID`), scoped topics,
persistence, multiple server instances.

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
never reads, the admission limit, and shutdown.
