package main

const labChooserHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Interaction lab</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Interaction lab</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p>Open the lab as one person in one window and as the other in a second window.</p>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/lab?as={{.ID}}">Open as {{.Name}}</a> ({{.Role.Label}})</li>
{{end}}</ul>
<p class="note">Choosing a person here is a demo switch, not a login.</p>
</body>
</html>
`

// The page script is application code: it shows each region's state,
// asks for a snapshot after held updates were discarded, keeps open
// editors through incoming swaps, and reports command outcomes.
const labPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Interaction lab as {{.P.Name}}</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
<script src="/static/tavern-0.0.24.min.js"></script>
</head>
<body hx-ext="sse" hx-vals='{"as": "{{.P.ID}}"}'>
<header>
  <h1>Interaction lab</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p class="persona">Working as <strong>{{.P.Name}}</strong> ({{.P.Role.Label}}). <a href="/showcase/lab?as={{.Other.ID}}">Switch to {{.Other.Name}}</a>.</p>
<p class="note">Updates keep arriving while you click and type. Each region holds them back while you interact with it, then catches up from the server.</p>

<form class="add" hx-post="/showcase/lab/tasks" hx-swap="none"
      hx-on::after-request="if (event.detail.successful) this.reset()">
  <input name="title" placeholder="New task" required maxlength="120" aria-label="New task">
  <button>Add task</button>
</form>
<p id="lab-command-status" role="status"></p>

<div class="lab">
  <section>
    <h2>Votes</h2>
    <p class="lab-state" id="lab-state-votes" data-state="connecting">Connecting…</p>
    <div id="lab-votes" class="lab-region" data-region="votes"
         sse-connect="/showcase/lab/votes/events?as={{.P.ID}}&amp;conn={{.Conn}}"
         tavern-hearth tavern-command-dedup="400" tavern-hot-policy="pause-on-pointerdown">
      <div sse-swap="lab-snapshot,lab-update" hx-swap="none"></div>
      <div id="lab-votes-list"></div>
    </div>
  </section>
  <section>
    <h2>Editor</h2>
    <p class="lab-state" id="lab-state-editor" data-state="connecting">Connecting…</p>
    <div id="lab-editor" class="lab-region" data-region="editor"
         sse-connect="/showcase/lab/editor/events?as={{.P.ID}}&amp;conn={{.Conn}}"
         tavern-hot-policy="pause-on-pointerdown defer-on-focus">
      <div sse-swap="lab-snapshot,lab-update" hx-swap="none"></div>
      <div id="lab-edit-list"></div>
    </div>
  </section>
  <section>
    <h2>Server log</h2>
    <div id="lab-log" data-region="log"
         sse-connect="/showcase/lab/log/events?as={{.P.ID}}&amp;conn={{.Conn}}"
         tavern-command-delegate="click" tavern-command-target="button[command-url]" tavern-command-dedup="1000">
      <div sse-swap="lab-snapshot,lab-update" hx-swap="none"></div>
      <p>
        <button type="button" command-url="/showcase/lab/burst?as={{.P.ID}}" command-count="8">Simulate 8 votes</button>
        <button type="button" command-url="/showcase/lab/log/clear?as={{.P.ID}}">Clear log</button>
      </p>
      <div id="lab-log-list"></div>
    </div>
  </section>
</div>

<section>
  <h2>In this window</h2>
  <ol id="lab-client-log" class="events"></ol>
</section>

<script>
(function () {
  var conn = "{{.Conn}}", as = "{{.P.ID}}";
  var history = document.getElementById("lab-client-log");
  var status = document.getElementById("lab-command-status");

  function note(text) {
    var li = document.createElement("li");
    li.textContent = new Date().toLocaleTimeString() + "  " + text;
    history.prepend(li);
    while (history.children.length > 12) history.lastChild.remove();
  }

  // Swap the editor's conflict, deleted and invalid responses.
  htmx.config.responseHandling = [
    {code: "204", swap: false},
    {code: "409", swap: true},
    {code: "404", swap: true},
    {code: "422", swap: true},
    {code: "[23]..", swap: true},
    {code: "[45]..", swap: false, error: true},
  ];

  // Region state. tavern-js discards updates it held back; when any were
  // discarded, ask the server for a snapshot on this region's own stream,
  // so nothing older can arrive after it. A snapshot that is applied is
  // newer than every update held before it, so it clears the need. One
  // request runs at a time; a need that arises meanwhile is asked for
  // when that request settles, unless a snapshot has met it by then or the
  // region is protected again (its release will ask).
  document.querySelectorAll(".lab-region").forEach(function (region) {
    var name = region.dataset.region;
    var state = document.getElementById("lab-state-" + name);
    var behind = false, asking = false, again = false, protecting = false;
    function show(s, text) { state.dataset.state = s; state.textContent = text; }

    function ask() {
      if (asking) { again = true; return; }
      asking = true;
      again = false;
      Tavern.command("/showcase/lab/resync?as=" + as + "&conn=" + conn + "&region=" + name, {})
        .then(function () { note(name + ": asked for a snapshot"); })
        .catch(function (err) { show("error", "Could not catch up: " + err.message); })
        .finally(function () {
          asking = false;
          if (again && behind && !protecting) ask();
        });
    }

    region.addEventListener("htmx:sseMessage", function (e) {
      if (e.detail.type !== "lab-snapshot") return;
      behind = false;
      show("live", "Up to date");
    });
    region.addEventListener("tavern:policy-activated", function (e) {
      protecting = true;
      show("paused", "Paused: updates held");
      note(name + ": " + e.detail.policy + " started");
    });
    region.addEventListener("tavern:policy-deactivated", function (e) {
      protecting = false;
      note(name + ": protection ended, " + e.detail.flushed + " held updates discarded");
      if (e.detail.flushed > 0) behind = true;
      if (!behind) { show("live", "Up to date"); return; }
      show("refreshing", "Catching up…");
      ask();
    });
  });

  // An open editor holds a draft that only its own Save or Cancel may end.
  // Incoming changes to its task are not swapped in; a snapshot of the
  // whole list keeps the editor in place of the task's new card.
  document.body.addEventListener("htmx:oobBeforeSwap", function (e) {
    var target = e.detail.target;
    var forms = target.matches("form.lab-editor") ? [target] : target.querySelectorAll("form.lab-editor");
    forms.forEach(function (form) {
      var card = form.closest('[id^="lab-edit-"]');
      var notice = form.querySelector("[data-live-notice]");
      if (card === target) {
        e.detail.shouldSwap = false;
        notice.textContent = "This task changed on the server. Saving will ask before replacing it.";
        return;
      }
      var incoming = e.detail.fragment.querySelector("#" + card.id);
      if (!incoming) {
        notice.textContent = "This task was deleted on the server.";
        card.querySelectorAll("[autofocus]").forEach(function (el) { el.removeAttribute("autofocus"); });
        e.detail.fragment.firstElementChild.append(card);
        return;
      }
      var rev = incoming.querySelector("[data-rev]");
      if (rev && rev.dataset.rev !== form.dataset.rev) {
        notice.textContent = "This task changed on the server. Saving will ask before replacing it.";
      }
      // htmx focuses [autofocus] in swapped content; a kept editor must not
      // take focus back from wherever the user moved it.
      card.querySelectorAll("[autofocus]").forEach(function (el) { el.removeAttribute("autofocus"); });
      incoming.replaceWith(card);
    });
  });

  // Command outcomes reach the document even when a live update has already
  // replaced the command's element; detail.element names it. "Accepted"
  // means the server took the command; the regions update from their streams.
  function describe(e) {
    return (e.detail.element.textContent.trim() || "Command") + " (" + e.detail.url + ")";
  }
  document.addEventListener("tavern:command-sent", function (e) {
    status.textContent = "Sending " + describe(e);
    note("sent " + e.detail.url);
  });
  document.addEventListener("tavern:command-success", function (e) {
    status.textContent = "Accepted: " + describe(e);
    note("accepted " + e.detail.url);
  });
  document.addEventListener("tavern:command-error", function (e) {
    status.textContent = "Failed: " + describe(e) + ": " + e.detail.error.message;
    note("failed " + e.detail.url + ": " + e.detail.error.message);
  });
})();
</script>
</body>
</html>
`

const labFragmentsHTML = `{{define "vote-card"}}<div id="lab-vote-{{.T.ID}}">{{template "vote-body" .}}</div>{{end}}
{{define "vote-body"}}<article class="task lab-vote">
  <span class="title">{{.T.Title}}</span>{{if .T.Internal}} <span class="badge">Internal</span>{{end}}
  <span class="votes"><strong data-votes>{{.T.Votes}}</strong> votes</span>
  <button type="button" command-url="/showcase/lab/tasks/{id}/vote?as={{.P.ID}}" command-id="{{.T.ID}}">Vote</button>
</article>{{end}}
{{define "edit-card"}}<div id="lab-edit-{{.T.ID}}">{{template "edit-body" .}}</div>{{end}}
{{define "edit-body"}}<article class="task" data-rev="{{.T.TitleRev}}">
  <span class="title">{{.T.Title}}</span>{{if .T.Internal}} <span class="badge">Internal</span>{{end}}
  <span class="moves">
    <button hx-get="/showcase/lab/tasks/{{.T.ID}}/edit" hx-target="#lab-edit-{{.T.ID}}">Edit</button>
    <button hx-post="/showcase/lab/tasks/{{.T.ID}}/delete" hx-target="#lab-edit-{{.T.ID}}" hx-swap="outerHTML">Delete</button>
  </span>
</article>{{end}}
{{define "editor"}}<form class="lab-editor" data-rev="{{.Rev}}" hx-post="/showcase/lab/tasks/{{.TaskID}}/title" hx-target="#lab-edit-{{.TaskID}}">
  {{- if .Conflict}}<p class="lab-editor-notice" role="alert">Someone saved “{{.T.Title}}” while you were editing. Your text is below; save it over theirs or keep theirs.</p>{{end}}
  {{- if .Deleted}}<p class="lab-editor-notice" role="alert">This task was deleted, so it cannot be saved. Your text is below.</p>{{end}}
  {{- if .Problem}}<p class="lab-editor-notice" role="alert">{{.Problem}}</p>{{end}}
  <p class="lab-editor-notice" data-live-notice aria-live="polite"></p>
  <input type="hidden" name="rev" value="{{.Rev}}">
  <label>Title <input name="title" value="{{.Draft}}" maxlength="120" required autofocus{{if .Deleted}} readonly{{end}}></label>
  {{- if .Deleted}}
  <button type="button" hx-on:click="this.closest('[id^=lab-edit-]').remove()">Close</button>
  {{- else}}
  <button>{{if .Conflict}}Save mine over theirs{{else}}Save{{end}}</button>
  <button type="button" hx-get="/showcase/lab/tasks/{{.TaskID}}/view" hx-target="#lab-edit-{{.TaskID}}">{{if .Conflict}}Keep theirs{{else}}Cancel{{end}}</button>
  {{- end}}
</form>{{end}}
{{define "log"}}<ol class="activity">{{range .}}<li>{{.}}</li>{{else}}<li class="empty">Nothing yet</li>{{end}}</ol>{{end}}`
