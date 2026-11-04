package main

import "html/template"

// The page script adds and removes regions itself and calls tavern-js's
// lifecycle functions from buttons, so their effects can be seen one at a
// time.
var clientPage = template.Must(template.New("client").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tavern-js lifecycle</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
<script src="/static/tavern-0.0.24.min.js"></script>
</head>
<body hx-ext="sse">
<header>
  <h1>tavern-js lifecycle</h1>
  <nav><a href="/showcase">Showcase</a></nav>
</header>
<p class="note">Regions added here are bound by tavern-js: it marks them live, flashes them when an update arrives and lists scoped regions in its registry. Tavern.destroy() stops binding new regions and empties the registry, but leaves already bound regions and their connections alone; htmx closes a connection when its region is removed. Tavern.init() binds again, including regions added meanwhile. Binding an element twice does nothing. {{if .Debug}}tavern-debug is on: tavern-js logs to the console.{{else}}<a href="/showcase/client?debug=1">Open with tavern-debug</a> to see tavern-js log to the console.{{end}}</p>
<p>
  <button type="button" data-act="add">Add a region</button>
  <button type="button" data-act="add-scoped">Add a scoped region</button>
  <button type="button" data-act="remove">Remove all regions</button>
  <button hx-post="/showcase/client/pulse" hx-swap="none">Send an update</button>
</p>
<p>
  <button type="button" data-act="destroy">Tavern.destroy()</button>
  <button type="button" data-act="init">Tavern.init()</button>
  <button type="button" data-act="scan">Tavern.scanAndBind()</button>
  <button type="button" data-act="rebind">Tavern.bind() the first region again</button>
</p>
<p role="status" aria-live="polite">Registry: <output id="client-registry">none</output> · Server subscriptions: <output id="client-subs">…</output> <button type="button" data-act="refresh">Refresh</button></p>
<div id="client-regions"></div>
<script>
(function () {
  var conn = "{{.Conn}}", debug = {{.Debug}}, max = {{.Max}}, n = 0;
  var list = document.getElementById("client-regions");
  function region(e) { return e.target.closest && e.target.closest(".client-region"); }
  function bump(el, sel) { var s = el.querySelector(sel); s.textContent = Number(s.textContent) + 1; }
  document.addEventListener("tavern:region-updated", function (e) { var el = region(e); if (el) bump(el, "[data-updates]"); });
  document.addEventListener("tavern:live", function (e) { var el = region(e); if (el) bump(el, "[data-live]"); });
  function refresh() {
    list.querySelectorAll(".client-region").forEach(function (el) {
      el.querySelector("[data-bound]").textContent = el._tavernBound ? "yes" : "no";
    });
    var names = Object.keys(Tavern.streams());
    document.getElementById("client-registry").textContent = names.length ? names.join(", ") : "none";
    fetch("/showcase/client/state").then(function (r) { return r.json(); }).then(function (s) {
      document.getElementById("client-subs").textContent = s.subscriptions;
    });
  }
  function add(scoped) {
    if (list.children.length >= max) return;
    n++;
    var el = document.createElement("section");
    el.className = "region client-region";
    el.id = "client-region-" + n;
    el.setAttribute("sse-connect", "/showcase/client/events?conn=" + conn + "-" + n);
    el.setAttribute("tavern-live-class", "is-live");
    el.setAttribute("tavern-updated-class", "is-updated");
    el.setAttribute("tavern-updated-ms", "{{.UpdateMs}}");
    if (scoped) {
      el.setAttribute("tavern-role", "scoped");
      el.setAttribute("tavern-scope", "region-" + n);
    }
    if (debug) el.setAttribute("tavern-debug", "");
    el.innerHTML = "<h2>Region " + n + (scoped ? " (scoped)" : "") + "</h2>" +
      '<p>Value: <output sse-swap="pulse">none yet</output></p>' +
      "<p>Updates seen: <span data-updates>0</span> · Times live: <span data-live>0</span> · Bound: <span data-bound>no</span></p>";
    list.append(el);
    htmx.process(el);
  }
  var acts = {
    add: function () { add(false); },
    "add-scoped": function () { add(true); },
    // Swapping the list's content out through htmx runs htmx's cleanup,
    // which closes each region's connection; removing the elements directly
    // (or with htmx.remove) would leave the connections open.
    remove: function () { htmx.swap(list, "", { swapStyle: "innerHTML" }); },
    destroy: function () { Tavern.destroy(); },
    init: function () { Tavern.init(); },
    scan: function () { Tavern.scanAndBind(); },
    rebind: function () { var first = list.querySelector(".client-region"); if (first) Tavern.bind(first); },
    refresh: function () {},
  };
  document.body.addEventListener("click", function (e) {
    var act = e.target.dataset && e.target.dataset.act;
    if (!act) return;
    acts[act]();
    setTimeout(refresh, 100);
  });
  refresh();
})();
</script>
</body>
</html>
`))
