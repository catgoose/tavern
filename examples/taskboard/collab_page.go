package main

const collabChooserHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Collaboration</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Collaboration</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p>Open this board as one person in one window and as the other in a second window.</p>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/collaboration?as={{.ID}}">Open as {{.Name}}</a> ({{.Role.Label}})</li>
{{end}}</ul>
<p class="note">Choosing a person here is a demo switch, not a login. A real application takes identity from its session and decides access on the server.</p>
</body>
</html>
`

const collabPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Collaboration as {{.P.Name}}</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
</head>
<body hx-ext="sse" hx-vals='{"as": "{{.P.ID}}"}'>
<header>
  <h1>Collaboration</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
  <p id="collab-status" data-state="connecting"><span class="live">Live</span><span class="down">Reconnecting…</span><span class="connecting">Connecting…</span></p>
</header>
<p class="persona">Viewing as <strong id="collab-persona">{{.P.Name}}</strong> ({{.P.Role.Label}}). <a href="/showcase/collaboration?as={{.Other.ID}}">Switch to {{.Other.Name}}</a>. This switch is for the demo; a real application takes identity from its session.</p>

<div id="collab-stream" sse-connect="/showcase/collaboration/events?as={{.P.ID}}&amp;conn={{.Conn}}"
     hx-on:htmx:sse-open="document.getElementById('collab-status').dataset.state = 'live'"
     hx-on:htmx:sse-error="document.getElementById('collab-status').dataset.state = 'down'">
  <div sse-swap="board,inbox-assigned,inbox-completed" hx-swap="none"></div>
</div>
<div id="collab-feed-stream" sse-connect="/showcase/collaboration/feed?as={{.P.ID}}">
  <div sse-swap="{{.FeedEvents}}" hx-swap="none"></div>
</div>

<form class="add" hx-post="/showcase/collaboration/tasks" hx-swap="none"
      hx-on::after-request="if (event.detail.successful) this.reset()">
  <input name="title" placeholder="What needs doing?" required maxlength="120" aria-label="New task">
  {{if .P.Role.SeesInternal}}<label><input type="checkbox" name="internal"> Internal (leads only)</label>{{end}}
  <button>Add task</button>
</form>
<p><button hx-post="/showcase/collaboration/clear-done" hx-swap="none">Clear finished tasks</button></p>
<div id="collab-editor" class="editor"></div>

<main class="board collab-board">
{{range .Columns}}  <section class="column" aria-label="{{index $.Titles .}}"><h2>{{index $.Titles .}}</h2><div id="collab-col-{{.}}"><p class="empty">Loading…</p></div></section>
{{end}}</main>

<aside class="collab-side">
  <section><h2>Your inbox</h2><div id="collab-inbox"></div></section>
  <section><h2>Counts</h2><div id="collab-stats"></div></section>
  <section><h2>Activity</h2><div id="collab-activity"></div></section>
</aside>

<details class="demo">
  <summary>Try a reconnect</summary>
  <p>Disconnect ends this window's board stream and holds its reconnect. Make changes in the other window, then reconnect: this window starts again from a fresh snapshot of the board and your inbox.</p>
  <button hx-post="/showcase/collaboration/drop?conn={{.Conn}}" hx-swap="none">Disconnect this window</button>
  <button hx-post="/showcase/collaboration/resume?conn={{.Conn}}" hx-swap="none">Reconnect</button>
</details>
</body>
</html>
`

const collabFragmentsHTML = `{{define "card"}}<div id="{{.ID}}">{{template "card-body" .}}</div>{{end}}
{{define "card-body"}}<article class="task{{if .T.Internal}} internal{{end}}">
  <p class="title">{{.T.Title}}{{if .T.Internal}} <span class="badge">Internal</span>{{end}}</p>
  <small>Added by {{.T.Creator.Name}}{{with .T.Assignee}} · assigned to <span class="assignee">{{.Name}}</span>{{end}}</small>
  <span class="moves">{{range .Columns}}{{if ne . $.T.Column}}<button hx-post="/showcase/collaboration/tasks/{{$.T.ID}}/move" name="to" value="{{.}}" hx-swap="none">{{index $.Titles .}}</button>{{end}}{{end}}</span>
  <span class="moves">{{range .Assignable}}<button hx-post="/showcase/collaboration/tasks/{{$.T.ID}}/assign" name="to" value="{{.ID}}" hx-swap="none">Assign {{.Name}}</button>{{end}}{{if .T.Assignee}}<button hx-post="/showcase/collaboration/tasks/{{.T.ID}}/assign" name="to" value="" hx-swap="none">Unassign</button>{{end}}</span>
  <span class="moves"><button hx-get="/showcase/collaboration/tasks/{{.T.ID}}/edit" hx-target="#collab-editor">Edit</button><button hx-post="/showcase/collaboration/tasks/{{.T.ID}}/delete" hx-swap="none">Delete</button></span>
</article>{{end}}
{{define "activity"}}<ol class="activity">{{range .}}<li>{{.}}</li>{{else}}<li class="empty">No changes yet</li>{{end}}</ol>{{end}}
{{define "stats"}}<p class="stats">{{range .Columns}}{{index $.Titles .}}: <strong data-column="{{.}}">{{index $.Counts .}}</strong> {{end}}</p>{{end}}
{{define "notice"}}<div id="{{.ID}}"><p class="notice" data-kind="{{.N.Kind}}">{{.N.Text}}</p></div>{{end}}
{{define "editor"}}<form hx-post="/showcase/collaboration/tasks/{{.ID}}/title" hx-target="#collab-editor">
  <label>Rename task <input name="title" value="{{.Title}}" required maxlength="120"></label>
  <button>Save</button>
  <button type="button" hx-on:click="document.getElementById('collab-editor').replaceChildren()">Cancel</button>
</form>{{end}}
{{define "editor-closed"}}{{end}}`
