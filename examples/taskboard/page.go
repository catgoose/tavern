package main

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Task board</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
</head>
<body hx-ext="sse" sse-connect="/events?conn={{.Conn}}"
      hx-on:htmx:sse-open="document.getElementById('status').dataset.state = 'live'"
      hx-on:htmx:sse-error="document.getElementById('status').dataset.state = 'down'">
<header>
  <h1>Task board</h1>
  <p id="status" data-state="connecting"><span class="live">Live</span><span class="down">Reconnecting…</span><span class="connecting">Connecting…</span></p>
</header>

<form class="add" hx-post="/tasks" hx-swap="none" hx-include="#viewer"
      hx-on::after-request="if (event.detail.successful) this.reset()">
  <input name="title" placeholder="What needs doing?" required maxlength="120" aria-label="New task">
  <button>Add task</button>
</form>
<label class="viewer">Shown as <input id="viewer" name="name" value="{{.Name}}" maxlength="40"></label>
<p class="note">Open this page in a second window to see changes arrive live. The name is for display only; it is not a login.</p>

<main class="board">
{{range .Columns}}  <section class="column" hx-get="/columns/{{.}}" hx-trigger="sse:ready, sse:column-{{.}}"><h2>{{index $.Titles .}}</h2><p class="empty">Loading…</p></section>
{{end}}</main>

<aside>
  <h2>Activity</h2>
  <ol class="activity" hx-get="/activity" hx-trigger="sse:ready, sse:activity"></ol>
</aside>

<details class="demo">
  <summary>Try a slow connection</summary>
  <p>Pause this window's updates, then add sample tasks. Once more than {{.Buffer}} changes are waiting, the server ends this window's stream instead of skipping one. Resume, and the window reconnects and reloads the board.</p>
  <button hx-post="/demo/pause?conn={{.Conn}}" hx-swap="none">Pause updates</button>
  <button hx-post="/demo/sample" hx-include="#viewer" hx-swap="none">Add 10 sample tasks</button>
  <button hx-post="/demo/resume?conn={{.Conn}}" hx-swap="none">Resume</button>
</details>
</body>
</html>
`

const fragmentsHTML = `{{define "column"}}<h2>{{index .Titles .Column}} <span class="count">{{len .Tasks}}</span></h2>
<ul>
{{- range $t := .Tasks}}
  <li class="task"><span class="title">{{$t.Title}}</span> <small>added by {{$t.By}}</small>
    <span class="moves">{{range $.Columns}}{{if ne . $t.Column}}<button hx-post="/tasks/{{$t.ID}}/move" name="to" value="{{.}}" hx-include="#viewer" hx-swap="none">{{index $.Titles .}}</button>{{end}}{{end}}</span>
  </li>
{{- else}}
  <li class="empty">Nothing here</li>
{{- end}}
</ul>
{{end}}
{{define "activity"}}{{range .}}<li>{{.}}</li>{{else}}<li class="empty">No changes yet</li>{{end}}{{end}}`
