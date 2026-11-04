package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The connections page shows what the broker holds and counts. Each window
// makes two subscriptions: the feed, named with the window's connection ID
// and persona, and the connection-event topic, which reports joins and
// leaves on other topics but not on itself. Counts are of subscriptions,
// not windows.
const (
	cnFeed   = "ops:feed"
	cnMeta   = "ops:meta"
	cnBurst  = 10
	cnFeedID = "conns-feed"
)

type connsLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry

	mu  sync.Mutex
	seq int
	log obsLog
}

func newConnsLab(o *ops) *connsLab {
	return &connsLab{broker: o.broker(
		tavern.WithBufferSize(16),
		tavern.WithConnectionEvents(cnMeta),
		tavern.WithMetrics(),
		tavern.WithObservability(tavern.ObservabilityConfig{PublishLatency: true, SubscriberLag: true, ConnectionDuration: true, TopicThroughput: true}),
	)}
}

func (l *connsLab) open(conn string, p *persona) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	return openAll(
		func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(cnFeed, tavern.SubWithMeta(tavern.SubscribeMeta{
				ID: conn, Meta: map[string]string{"persona": p.id, "page": "conns"},
			}))
			return subOrErr("feed", ch, unsub)
		},
		func() (keyedSub, error) {
			ch, unsub := l.broker.Subscribe(cnMeta)
			return subOrErr("meta", ch, unsub)
		},
	)
}

// connsFrame sends connection events as "conn-event" events with their JSON
// payload, and feed updates as OOB HTML.
func connsFrame(m tavern.TopicMessage) string {
	if m.Topic == "meta" {
		return tavern.NewSSEMessage("conn-event", m.Data).String()
	}
	return deliveryFrame(m)
}

func (l *connsLab) burst() {
	for range cnBurst {
		l.mu.Lock()
		l.seq++
		n := l.seq
		l.mu.Unlock()
		l.broker.PublishOOB(cnFeed, tavern.Replace(cnFeedID, fmt.Sprintf(`<span data-seq="%d">message %d</span>`, n, n)))
	}
}

func durations(ds []time.Duration) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Round(time.Millisecond).String())
	}
	return out
}

func (l *connsLab) state() map[string]any {
	b := l.broker
	var subs []map[string]any
	for _, s := range b.Subscribers(cnFeed) {
		subs = append(subs, map[string]any{"id": s.ID, "meta": s.Meta, "connectedFor": time.Since(s.ConnectedAt).Round(time.Second).String()})
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i]["id"].(string) < subs[j]["id"].(string) })
	m := b.Metrics()
	obs := b.Observability().Snapshot(b)
	topics := map[string]any{}
	for t, o := range obs.Topics {
		topics[t] = map[string]any{
			"publishLatency":        map[string]string{"p50": o.PublishLatency.P50.String(), "p95": o.PublishLatency.P95.String(), "p99": o.PublishLatency.P99.String()},
			"latencySamples":        o.PublishLatency.Count,
			"subscriberQueueDepth":  o.SubscriberLag,
			"closedConnectionTimes": durations(o.ConnectionDurations),
			"messagesPerSecond":     o.Throughput,
			"evictions":             o.EvictionCount,
		}
	}
	stats := b.Stats()
	l.mu.Lock()
	seq := l.seq
	l.mu.Unlock()
	return map[string]any{
		"lastMessage":     seq,
		"feedSubscribers": subs,
		"hasSubscribers":  b.HasSubscribers(cnFeed),
		"subscriptions":   b.SubscriberCount(),
		"topicCounts":     b.TopicCounts(),
		"stats":           map[string]any{"topics": stats.Topics, "subscriptions": stats.Subscribers, "drops": stats.PublishDrops, "replayStoreErrors": stats.ReplayStoreErrors},
		"metrics":         map[string]any{"topics": m.TopicStats, "totalPublished": m.TotalPublished, "totalDropped": m.TotalDropped},
		"observability":   topics,
		"observations":    l.log.list(),
	}
}

func (a *App) registerConns() {
	l := a.ops.conns
	a.mux.HandleFunc("GET /showcase/ops/conns", a.withPersona(func(w http.ResponseWriter, _ *http.Request, p *persona) {
		a.renderOpsPage(w, connsPage, map[string]any{"Title": "Connections and metrics", "Family": "conns", "P": p})
	}))
	a.mux.HandleFunc("GET /showcase/ops/conns/events", a.withPersona(func(w http.ResponseWriter, r *http.Request, p *persona) {
		a.serveOwned(w, r, ownedStream{
			reg:   &l.reg,
			open:  func(conn string) (<-chan tavern.TopicMessage, func(), error) { return l.open(conn, p) },
			frame: connsFrame,
		})
	}))
	a.mux.HandleFunc("POST /showcase/ops/conns/burst", func(w http.ResponseWriter, _ *http.Request) {
		l.burst()
		w.WriteHeader(http.StatusNoContent)
	})
	// disconnect closes this window's feed subscription by its ID, which
	// ends the window's stream; the browser reconnects.
	a.mux.HandleFunc("POST /showcase/ops/conns/disconnect", func(w http.ResponseWriter, r *http.Request) {
		conn := r.URL.Query().Get("conn")
		if l.reg.owner(conn) == nil || !l.broker.Disconnect(cnFeed, conn) {
			http.Error(w, "This window has no feed subscription", http.StatusNotFound)
			return
		}
		l.log.add("disconnected %s", conn)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/ops/conns/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, l.state())
	})
}

var connsPage = deliveryPage("conns", `{{define "main"}}
<p class="note">This window has two subscriptions: the feed, named with its connection ID and your persona, and the connection-event topic. Connection events report subscriptions joining and leaving other topics, with the count after the change; they do not name the subscriber, and the event topic does not report itself. Counts below are of subscriptions, not windows. Metrics are cumulative since the server started; the queue depth and message rate are samples.</p>
<section id="conns-region" sse-connect="/showcase/ops/conns/events?as={{.P.ID}}&amp;conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <p>Feed: <span id="conns-feed">nothing yet</span></p>
  <h2>Connection events</h2>
  <ol id="conns-events" class="events" sse-swap="conn-event" hx-swap="none"></ol>
</section>
<p>
  <button hx-post="/showcase/ops/conns/burst" hx-swap="none">Publish 10 messages</button>
  <button hx-post="/showcase/ops/conns/disconnect?conn={{.Conn}}" hx-swap="none">Disconnect this window</button>
</p>
<p class="note">Connection ID of this window: <code id="conns-id">{{.Conn}}</code></p>
<script>
document.body.addEventListener("htmx:sseMessage", function (e) {
  if (e.detail.type !== "conn-event") return;
  var list = document.getElementById("conns-events");
  var li = document.createElement("li");
  li.textContent = e.detail.data;
  list.prepend(li);
  while (list.children.length > 10) list.lastChild.remove();
});
</script>
{{end}}`)
