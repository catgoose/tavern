package main

const showcaseHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Showcase</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Showcase</h1>
  <nav><a href="/">Task board</a></nav>
</header>
<p>Working scenarios for Tavern and tavern-js. Each one runs against this server.</p>
<ul class="scenarios">
  <li><a href="/showcase/connection">Connection and live updates</a>: tavern-js connection state, a live counter, and reconnecting with replay.</li>
</ul>
<p class="note">The task board itself shows invalidation streams, loss-aware subscriptions and graceful shutdown.</p>
</body>
</html>
`

// The scripts load in dependency order: htmx, its SSE extension, then
// tavern-js, which binds to sse-connect elements and listens for the
// extension's events.
const connectionHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connection and live updates</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
<script src="/static/tavern-0.0.23.min.js"></script>
</head>
<body>
<header>
  <h1>Connection and live updates</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>

<section id="counter-region" class="region" hx-ext="sse" sse-connect="/showcase/connection/events?conn={{.Conn}}"
         tavern-live-class="is-live" tavern-stale-class="is-stale"
         tavern-reconnecting-class="is-reconnecting"
         tavern-updated-class="is-updated" tavern-updated-ms="800">
  <p class="state" aria-live="polite">
    <span class="connecting">Connecting…</span>
    <span tavern-status-live hidden>Live</span>
    <span tavern-status hidden>Connection lost.</span>
    <span tavern-status-recovering hidden>Reconnected; waiting for the server to confirm the replay.</span>
    <span tavern-status-stale hidden>Stale: some updates were missed.</span>
  </p>
  <p class="counter">Count: <output id="count" sse-swap="counter">{{.Count}}</output></p>
</section>

<p>
  <button hx-post="/showcase/connection/increment" hx-swap="none">Increment</button>
  <button hx-post="/showcase/connection/drop?conn={{.Conn}}" hx-swap="none">Drop connection</button>
  <button hx-post="/showcase/connection/drop?conn={{.Conn}}&amp;away=1" hx-swap="none">Drop, then increment while away</button>
</p>
<p class="note">Increment publishes the new count with an event ID, and every open window updates. Dropping ends this window's stream on the server. The browser reconnects with the last event ID it saw; the server replays what it missed and sends <code>tavern-reconnected</code>, and only then does the region show Live again.</p>

<h2>tavern-js events</h2>
<ol id="events" class="events"></ol>
<script>
(function () {
  var log = document.getElementById("events");
  ["tavern:live", "tavern:disconnected", "tavern:transport-closed", "tavern:transport-open",
   "tavern:recovering", "tavern:reconnected", "tavern:stale", "tavern:replay-gap",
   "tavern:replay-truncated", "tavern:region-updated"].forEach(function (name) {
    document.addEventListener(name, function (e) {
      var li = document.createElement("li");
      li.dataset.event = name;
      var detail = e.detail && Object.keys(e.detail).length ? " " + JSON.stringify(e.detail) : "";
      li.textContent = new Date().toLocaleTimeString() + "  " + name + detail;
      log.prepend(li);
      while (log.children.length > 30) log.lastChild.remove();
    });
  });
})();
</script>
</body>
</html>
`
