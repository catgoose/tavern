package main

const recoveryIndexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Recovery lab</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Recovery lab</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p>Each scenario runs on its own broker and has one value shared by every window. The buttons that cause a loss act on your window's connection only.</p>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/recovery/{{.Name}}">{{.Title}}</a>{{if eq .Name "resume"}}: gap action
    <a href="/showcase/recovery/resume?action=banner">banner</a>,
    <a href="/showcase/recovery/resume?action=reload">reload</a>,
    <a href="/showcase/recovery/resume?action=recovery-gap">custom event</a>{{end}}</li>
{{end}}</ul>
</body>
</html>
`

// The page script is application code: it shows tavern-js's events and,
// for the custom gap action, fetches the current value from the server.
const recoveryPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.S.Title}}</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
<script src="/static/tavern-0.0.24.min.js"></script>
</head>
<body>
<header>
  <h1>{{.S.Title}}</h1>
  <nav><a href="/showcase/recovery">Recovery lab</a> · <a href="/showcase">Showcase</a></nav>
</header>
<p class="note">{{.S.About}}</p>

<section id="recovery-region" class="region" hx-ext="sse"
         sse-connect="/showcase/recovery/{{.S.Name}}/events?conn={{.Conn}}"
         tavern-live-class="is-live" tavern-stale-class="is-stale" tavern-reconnecting-class="is-reconnecting"
         {{with .Action}}tavern-gap-action="{{.}}"{{end}}
         tavern-gap-banner-text="Some updates were missed while this window was away.">
  <p class="state" aria-live="polite">
    <span class="connecting">Connecting…</span>
    <span tavern-status-live hidden>Live</span>
    <span tavern-status hidden>Connection lost.</span>
    <span tavern-status-recovering hidden>Reconnected; waiting for the server to confirm the replay.</span>
    <span tavern-status-stale hidden>Stale: some updates were missed.</span>
  </p>
  {{if .Multi}}
  <p class="counter">Left: <output id="recovery-left" sse-swap="left">…</output> · Right: <output id="recovery-right" sse-swap="right">…</output></p>
  {{else}}
  <p class="counter">Value: <output id="recovery-value" sse-swap="value">…</output></p>
  {{end}}
</section>

<p>
  {{if .Multi}}<button hx-post="/showcase/recovery/{{.S.Name}}/change?topic=recovery/left" hx-swap="none">Change left</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/change?topic=recovery/right" hx-swap="none">Change right</button>
  {{else}}<button hx-post="/showcase/recovery/{{.S.Name}}/change" hx-swap="none">Change</button>{{end}}
  {{if .Pressure}}
  <button hx-post="/showcase/recovery/{{.S.Name}}/change?n=10" hx-swap="none">Make 10 changes</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/pause?conn={{.Conn}}&amp;on=1" hx-swap="none">Hold back this window</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/pause?conn={{.Conn}}&amp;on=0" hx-swap="none">Release it</button>
  {{else}}
  <button hx-post="/showcase/recovery/{{.S.Name}}/away?conn={{.Conn}}&amp;n=3" hx-swap="none">Disconnect and miss 3 changes</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/away?conn={{.Conn}}&amp;n=8" hx-swap="none">Disconnect and miss 8 changes</button>
  {{if eq .S.Name "truncation"}}<button hx-post="/showcase/recovery/{{.S.Name}}/away?conn={{.Conn}}&amp;n=10" hx-swap="none">Disconnect and miss 10 changes</button>{{end}}
  <button hx-post="/showcase/recovery/{{.S.Name}}/away?conn={{.Conn}}&amp;n=0" hx-swap="none">Reconnect now</button>
  {{end}}
</p>
{{if .Store}}
<p>The replay store is shared by both store scenarios and every window.
  <button hx-post="/showcase/recovery/{{.S.Name}}/fault?append=1" hx-swap="none">Fail appends</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/fault?read=1" hx-swap="none">Fail reads</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/fault?append=0&amp;read=0" hx-swap="none">Repair the store</button>
  <button hx-post="/showcase/recovery/{{.S.Name}}/clear" hx-swap="none">Clear replay history</button>
</p>
{{end}}

<section>
  <h2>What happened</h2>
  <p><button type="button" id="recovery-inspect">Show what the server saw</button> <span id="recovery-server"></span></p>
  <ol id="recovery-observations" class="events"></ol>
  <h3>In this window</h3>
  <ol id="recovery-events" class="events"></ol>
</section>

<script>
(function () {
  var name = "{{.S.Name}}", conn = "{{.Conn}}";
  var log = document.getElementById("recovery-events");
  function note(text) {
    var li = document.createElement("li");
    li.textContent = new Date().toLocaleTimeString() + "  " + text;
    log.prepend(li);
    while (log.children.length > 30) log.lastChild.remove();
  }
  ["tavern:live", "tavern:disconnected", "tavern:recovering", "tavern:reconnected", "tavern:stale",
   "tavern:replay-gap", "tavern:replay-truncated", "recovery-gap"].forEach(function (type) {
    document.addEventListener(type, function (e) {
      var detail = e.detail && Object.keys(e.detail).length ? " " + JSON.stringify(e.detail) : "";
      note(type + detail);
    });
  });

  function state() {
    return fetch("/showcase/recovery/" + name + "/state?conn=" + conn).then(function (r) { return r.json(); });
  }
  // seen is the newest event ID the stream has applied. Every value message
  // carries one, so a fetched state is applied only if it is newer.
  var seen = 0;
  document.addEventListener("htmx:sseMessage", function (e) {
    var id = Number(e.detail && e.detail.lastEventId);
    if (id > seen) seen = id;
  });
  function show(values) {
    Object.keys(values).forEach(function (topic) {
      var id = { "recovery/left": "recovery-left", "recovery/right": "recovery-right" }[topic] || "recovery-value";
      document.getElementById(id).textContent = values[topic];
    });
  }

  // The custom gap action: fetch the current values from the server. One
  // fetch at a time; a newer gap cancels the older one. A failed fetch is
  // tried twice more, then left to the stream.
  var pending = null;
  document.addEventListener("recovery-gap", function () {
    if (pending) pending.abort();
    var ctl = new AbortController();
    pending = ctl;
    var tries = 0;
    (function attempt() {
      if (ctl.signal.aborted) return;
      fetch("/showcase/recovery/" + name + "/state?conn=" + conn, { signal: ctl.signal })
        .then(function (r) {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        })
        .then(function (s) {
          if (pending !== ctl) return;
          pending = null;
          if (s.rev <= seen) {
            note("custom handler: fetched revision " + s.rev + ", already showing " + seen);
            return;
          }
          show(s.values);
          seen = s.rev;
          note("custom handler: applied revision " + s.rev);
        })
        .catch(function (err) {
          if (pending !== ctl) return;
          if (++tries < 3) {
            setTimeout(attempt, 500 * tries);
            return;
          }
          pending = null;
          note("custom handler: gave up (" + err.message + "); the stream stays authoritative");
        });
    })();
  });
  document.getElementById("recovery-inspect").addEventListener("click", function () {
    state().then(function (s) {
      var store = s.failAppend || s.failRead
        ? " · store failing: " + [s.failAppend && "appends", s.failRead && "reads"].filter(Boolean).join(", ")
        : "";
      document.getElementById("recovery-server").textContent = "Server value: " + JSON.stringify(s.values) + store;
      var list = document.getElementById("recovery-observations");
      list.replaceChildren();
      s.observations.forEach(function (o) {
        var li = document.createElement("li");
        li.textContent = o.text;
        list.append(li);
      });
    });
  });
})();
</script>
</body>
</html>
`
