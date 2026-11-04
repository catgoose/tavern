package main

const shellChooserHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>App shell</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>App shell</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/shell?as={{.ID}}">Open as {{.Name}}</a> ({{.Role.Label}})</li>
{{end}}</ul>
<p class="note">Choosing a person here is a demo switch, not a login.</p>
</body>
</html>
`

// The page script is application code. tavern-js registers the lifeline and
// each view's scoped stream; this script promotes a view once its own first
// data has arrived, retires it when it is swapped out (htmx then closes its
// connection), and shows what the library reports.
const shellPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>App shell as {{.P.Name}}</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
<script src="/static/tavern-0.0.24.min.js"></script>
</head>
<body hx-ext="sse" hx-vals='{"as": "{{.P.ID}}"}'>
<header>
  <h1>App shell</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p class="persona">Working as <strong>{{.P.Name}}</strong> ({{.P.Role.Label}}). The lifeline below stays connected while you switch views; each view has its own stream.</p>

<div id="shell-lifeline" class="shell-lifeline" tavern-role="lifeline"
     sse-connect="/showcase/shell/lifeline?as={{.P.ID}}&amp;conn={{.Conn}}">
  <div sse-swap="shell/notices,shell/role,shell/channel/releases,shell/channel/incidents,shell/channel/leads" hx-swap="none"></div>
  <section>
    <h2>Notices</h2>
    <div id="shell-notices"></div>
    <button hx-post="/showcase/shell/notices" hx-swap="none">Post a notice</button>
    <p>Note to your role: <span id="shell-role-notice">none yet</span></p>
    <button hx-post="/showcase/shell/role-note?to=lead" hx-swap="none">Note to leads</button>
    <button hx-post="/showcase/shell/role-note?to=contributor" hx-swap="none">Note to contributors</button>
  </section>
  <section>
    <h2>Channels</h2>
    <p>Lifeline topics: <span id="shell-topics">shell/notices</span></p>
    <div class="shell-channel"><strong>releases</strong> <span id="shell-channel-releases">not followed</span>
      <button hx-post="/showcase/shell/follow?conn={{.Conn}}&amp;channel=releases&amp;on=1" hx-swap="none">Follow</button>
      <button hx-post="/showcase/shell/follow?conn={{.Conn}}&amp;channel=releases&amp;on=0" hx-swap="none">Unfollow</button>
      <button hx-post="/showcase/shell/channels/releases" hx-swap="none">Post</button></div>
    <div class="shell-channel"><strong>incidents</strong> <span id="shell-channel-incidents">not followed</span>
      <button hx-post="/showcase/shell/follow?conn={{.Conn}}&amp;channel=incidents&amp;on=1" hx-swap="none">Follow</button>
      <button hx-post="/showcase/shell/follow?conn={{.Conn}}&amp;channel=incidents&amp;on=0" hx-swap="none">Unfollow</button>
      <button hx-post="/showcase/shell/channels/incidents" hx-swap="none">Post</button></div>
    {{if .Lead}}<div class="shell-channel"><strong>leads</strong> <span id="shell-channel-leads">not added</span>
      <button hx-post="/showcase/shell/leads?on=1" hx-swap="none">Add for every lead</button>
      <button hx-post="/showcase/shell/leads?on=0" hx-swap="none">Remove for every lead</button>
      <button hx-post="/showcase/shell/channels/leads" hx-swap="none">Post</button></div>{{end}}
  </section>
</div>

<nav id="shell-nav" class="shell-nav" hx-target="#shell-view" hx-sync="this:replace">
  <button hx-get="/showcase/shell/views/board?conn={{.Conn}}">Board</button>
  <button hx-get="/showcase/shell/views/feeds?conn={{.Conn}}">Feeds</button>
  <button hx-get="/showcase/shell/views/ops?conn={{.Conn}}">Ops</button>
</nav>
<p id="shell-fallback" role="status"></p>
<div id="shell-view" hx-get="/showcase/shell/views/board?conn={{.Conn}}" hx-trigger="load"></div>

<section>
  <h2>Streams in this window</h2>
  <p><button type="button" id="shell-inspect">Show server state</button></p>
  <pre id="shell-server"></pre>
  <ol id="shell-log" class="events"></ol>
</section>

<script>
(function () {
  var conn = "{{.Conn}}", as = "{{.P.ID}}";
  var log = document.getElementById("shell-log");
  var fallback = document.getElementById("shell-fallback");
  function note(text) {
    var li = document.createElement("li");
    li.textContent = new Date().toLocaleTimeString() + "  " + text;
    log.prepend(li);
    while (log.children.length > 20) log.lastChild.remove();
  }
  function current() { return document.querySelector("#shell-view [tavern-role=scoped]"); }

  ["tavern:stream-warming", "tavern:stream-ready", "tavern:stream-promoted", "tavern:stream-retired"].forEach(function (name) {
    document.addEventListener(name, function (e) { note(name.slice(14) + " " + e.detail.scope); });
  });

  // Promote a view only when its own first data has arrived, and only if it
  // is still the view on screen: a socket opening is not a loaded view.
  document.body.addEventListener("htmx:sseMessage", function (e) {
    var view = e.target.closest("[tavern-role=scoped]");
    if (!view || (view.dataset.readyEvents || "").split(" ").indexOf(e.detail.type) === -1) return;
    var scope = view.getAttribute("tavern-scope");
    var entry = Tavern.stream(scope);
    if (view !== current() || !entry || entry.el !== view || entry.state === "active") return;
    Tavern.promote(scope);
    if (fallback.dataset.scope === scope) { fallback.textContent = ""; delete fallback.dataset.scope; }
  });

  // Leaving a view retires its registry entry; htmx removes the element and
  // closes its connection. Requests from buttons inside the view also
  // bubble here, so act only when the view container itself is replaced.
  var viewBox = document.getElementById("shell-view");
  viewBox.addEventListener("htmx:beforeSwap", function (e) {
    if (e.detail.target !== viewBox) return;
    var view = current();
    if (view) Tavern.retire(view.getAttribute("tavern-scope"));
  });

  var lifeline = document.getElementById("shell-lifeline");
  lifeline.addEventListener("tavern:stream-fallback", function (e) {
    fallback.dataset.scope = e.detail.scope;
    fallback.textContent = "The " + e.detail.scope + " view lost its stream; it is reconnecting. Notices still arrive on the lifeline.";
    note("fallback " + e.detail.scope);
  });
  lifeline.addEventListener("tavern:topics-changed", function (e) {
    var topics = (e.detail.topics || []).slice().sort();
    document.getElementById("shell-topics").textContent = topics.join(", ");
    if (e.detail.action === "removed") {
      var panel = document.getElementById(e.detail.topic.split("/").join("-"));
      if (panel) panel.textContent = "not followed";
    }
    note("topics " + e.detail.action + " " + e.detail.topic);
  });

  document.getElementById("shell-inspect").addEventListener("click", function () {
    fetch("/showcase/shell/diagnostics?as=" + as + "&conn=" + conn)
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var s = Tavern.streams(), names = Object.keys(s).map(function (k) { return k + ":" + s[k].state; });
        document.getElementById("shell-server").textContent =
          "lifeline topics: " + d.lifelineTopics.join(", ") + "\n" +
          "board streams: " + d.boardStreams + "\n" +
          "registered views: " + names.join(", ") + "\n" +
          "subscribers: " + JSON.stringify(d.topicCounts);
      });
  });
})();
</script>
</body>
</html>
`

const shellFragmentsHTML = `{{define "notices"}}<ol class="activity">{{range .}}<li>{{.}}</li>{{else}}<li class="empty">No notices yet</li>{{end}}</ol>{{end}}
{{define "channel"}}{{.}}{{end}}
{{define "text"}}{{.}}{{end}}
{{define "board-tasks"}}<ul>{{range .}}<li>{{.}}</li>{{else}}<li class="empty">No tasks yet</li>{{end}}</ul>{{end}}
{{define "feed-item"}}<li id="{{.ID}}"><code>{{.Topic}}</code> {{.Text}}</li>{{end}}
{{define "view-board"}}<section class="shell-view" tavern-role="scoped" tavern-scope="board" data-ready-events="shell-board-snapshot"
     sse-connect="/showcase/shell/views/board/events?as={{.P.ID}}&amp;conn={{.Conn}}">
  <div sse-swap="shell-board-snapshot,shell/board/tasks,shell/board/stats" hx-swap="none"></div>
  <h2>Board</h2>
  <p class="note">One stream over two topics (SubscribeMultiWith), starting from a snapshot. If it falls behind it ends and restarts from a new snapshot.</p>
  <p id="shell-board-stats">Loading…</p>
  <div id="shell-board-tasks"></div>
  <button hx-post="/showcase/shell/board/tasks" hx-swap="none">Add a task</button>
  <button hx-post="/showcase/shell/views/board/break?conn={{.Conn}}" hx-swap="none">Break this view's stream</button>
  <button hx-post="/showcase/shell/views/board/recover?conn={{.Conn}}" hx-swap="none">Let it reconnect</button>
</section>{{end}}
{{define "view-feeds"}}<section class="shell-view" tavern-role="scoped" tavern-scope="feeds" data-ready-events="shell-feed-ready"
     sse-connect="/showcase/shell/views/feeds/events?as={{.P.ID}}&amp;conn={{.Conn}}">
  <div sse-swap="shell-feed-ready,shell-feed" hx-swap="none"></div>
  <h2>Feeds</h2>
  <p class="note">One glob subscription to <code>shell/feeds/**</code>. Items are live only: this view starts empty and shows what is published while it is open, labelled with the topic it came from.</p>
  <button hx-post="/showcase/shell/feeds/web" hx-swap="none">Publish to shell/feeds/releases/web</button>
  <button hx-post="/showcase/shell/feeds/api" hx-swap="none">Publish to shell/feeds/releases/api</button>
  <button hx-post="/showcase/shell/feeds/noise" hx-swap="none">Publish to shell/other/noise</button>
  <ol id="shell-feed-list"></ol>
</section>{{end}}
{{define "view-ops"}}<section class="shell-view" tavern-role="scoped" tavern-scope="ops" data-ready-events="shell/ops/deploys shell/ops/alerts"
     sse-connect="/showcase/shell/views/ops/events?as={{.P.ID}}">
  <div sse-swap="shell/ops/deploys,shell/ops/alerts" hx-swap="none"></div>
  <h2>Ops</h2>
  <p class="note">A static topic group (DefineGroup and GroupHandler) over two topics; each event is named after its topic. Each topic keeps its latest value, so the view starts from it.</p>
  <p><span id="shell-ops-deploys">…</span> · <span id="shell-ops-alerts">…</span></p>
  <button hx-post="/showcase/shell/ops/deploy" hx-swap="none">Record a deploy</button>
  <button hx-post="/showcase/shell/ops/alert" hx-swap="none">Raise an alert</button>
</section>{{end}}`
