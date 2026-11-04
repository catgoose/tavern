package main

import (
	"html/template"
	"net/http"
)

// The operations lab shows what the server observes and how it bounds and
// cleans up: presence across tabs, connection events and metrics,
// admission limits, transport lifetimes and write failures, and shutting
// down an owned child broker. Each page has its own brokers.
type ops struct {
	labRuntime

	presence *presenceLab
	conns    *connsLab
	admit    *admitLab
	wire     *wireLab
	child    *childLab
}

func newOps() *ops {
	o := &ops{labRuntime: newLabRuntime()}
	o.presence = newPresenceLab(o)
	o.conns = newConnsLab(o)
	o.admit = newAdmitLab(o)
	o.wire = newWireLab(o)
	o.child = newChildLab(o)
	return o
}

func (o *ops) close() {
	o.child.shutdown()
	o.admit.release()
	o.presence.tracker.Close()
	o.stop()
	o.closeBrokers()
}

func (a *App) registerOps() {
	a.ops = newOps()
	a.mux.HandleFunc("GET /showcase/ops", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := opsIndexTmpl.Execute(w, opsPages); err != nil {
			a.logger.Error("render ops index", "err", err)
		}
	})
	a.registerPresence()
	a.registerConns()
	a.registerAdmit()
	a.registerWire()
	a.registerChild()
}

// renderOpsPage renders one operations page in the shared lab layout.
func (a *App) renderOpsPage(w http.ResponseWriter, t *template.Template, data map[string]any) {
	data["Base"], data["Lab"] = "/showcase/ops", "Operations lab"
	a.renderDeliveryPage(w, t, data)
}

var opsPages = []deliveryFamily{
	{"presence", "Presence across tabs", "Who is here, per person, with two tabs of one person, heartbeats and stale removal."},
	{"conns", "Connections and metrics", "Connection events, the subscribers the broker holds, a targeted disconnect, and broker metrics."},
	{"admit", "Admission limits", "Global, per-topic and custom limits refusing subscriptions, and capacity returning."},
	{"wire", "Transport lifetimes and write failures", "Heartbeats, keepalives, finite connections with a retry value, and how write failures are reported."},
	{"child", "Shutting down a broker", "An owned child broker with live, held and slow streams and a publisher, shut down and started again."},
}

var opsIndexTmpl = template.Must(template.New("ops-index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Operations lab</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Operations lab</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p>Each page has its own brokers, so its limits and failures do not reach the rest of the app. The backend and exporter commands are listed in the task board README.</p>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/ops/{{.Path}}">{{.Title}}</a>: {{.About}}</li>
{{end}}</ul>
</body>
</html>
`))
