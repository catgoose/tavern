package main

import (
	"context"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The shaping family runs one fast producer against slow windows. Each panel
// is its own stream: plain, rate-limited (latest wins once per interval) or
// coalescing (the reader always gets the latest value). Every write to these
// windows waits shSlow, so a plain stream falls behind and drops.
const (
	shTopic    = "delivery/ticker"
	shInterval = 200 * time.Millisecond
	shSlow     = 60 * time.Millisecond
	shSpacing  = 5 * time.Millisecond
	shBurst    = 50
)

// shapingModes are the subscriptions a panel can use.
var shapingModes = map[string]func(b *tavern.SSEBroker, meta tavern.SubscribeOption) (<-chan string, func()){
	"plain": func(b *tavern.SSEBroker, meta tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWith(shTopic, meta)
	},
	"rate": func(b *tavern.SSEBroker, meta tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWith(shTopic, meta, tavern.SubWithRate(tavern.Rate{MinInterval: shInterval}))
	},
	"coalesce": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWithCoalescing(shTopic)
	},
	// The named forms: a rate given per second, and the team-scoped variants.
	"rate-per-second": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWithRate(shTopic, tavern.Rate{MaxPerSecond: 5})
	},
	"team-rate": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeScopedWithRate(shTopic, deliveryTeam, tavern.Rate{MinInterval: shInterval})
	},
	"team-coalesce": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeScopedWithCoalescing(shTopic, deliveryTeam)
	},
	// Unshaped named forms: metadata, and a filter that passes every tenth
	// value (the burst's last value is one).
	"meta": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWithMeta(shTopic, tavern.SubscribeMeta{ID: "named-meta"})
	},
	"tenths": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeWithFilter(shTopic, everyTenth)
	},
	"team-tenths": func(b *tavern.SSEBroker, _ tavern.SubscribeOption) (<-chan string, func()) {
		return b.SubscribeScopedWithFilter(shTopic, deliveryTeam, everyTenth)
	},
}

func everyTenth(msg string) bool {
	n, err := strconv.Atoi(msg)
	return err == nil && n%10 == 0
}

type shapingLab struct {
	d      *delivery
	broker *tavern.SSEBroker
	reg    connRegistry

	mu       sync.Mutex
	value    int
	produced int
	written  map[string]int // conn → values written to it
	running  atomic.Bool
}

func newShapingLab(d *delivery) *shapingLab {
	return &shapingLab{d: d, broker: d.broker(tavern.WithBufferSize(8)), written: map[string]int{}}
}

// burst publishes shBurst values to everyone and to the red team.
func (l *shapingLab) burst() bool {
	if !l.running.CompareAndSwap(false, true) {
		return false
	}
	l.d.goBounded(func(ctx context.Context) {
		defer l.running.Store(false)
		for i := range shBurst {
			if i > 0 && !sleepCtx(ctx, shSpacing) {
				return
			}
			l.mu.Lock()
			l.value++
			l.produced++
			v := strconv.Itoa(l.value)
			l.mu.Unlock()
			l.broker.Publish(shTopic, v)
			l.broker.PublishTo(shTopic, deliveryTeam, v)
		}
	})
	return true
}

func (l *shapingLab) opener(mode string) func(conn string) (<-chan tavern.TopicMessage, func(), error) {
	return func(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
		return openAll(func() (keyedSub, error) {
			ch, unsub := shapingModes[mode](l.broker, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn}))
			return subOrErr(mode, ch, unsub)
		})
	}
}

func (l *shapingLab) onRead(conn string, _ tavern.TopicMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.written[conn]++
}

func (l *shapingLab) ended(conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.written, conn)
}

func (a *App) registerShaping() {
	l := a.delivery.shaping
	a.mux.HandleFunc("GET /showcase/delivery/shaping", func(w http.ResponseWriter, r *http.Request) {
		modes := []string{"plain", "rate", "coalesce"}
		switch r.URL.Query().Get("named") {
		case "1":
			modes = []string{"rate-per-second", "team-rate", "team-coalesce"}
		case "2":
			modes = []string{"meta", "tenths", "team-tenths"}
		}
		a.renderDeliveryPage(w, shapingPage, map[string]any{"Title": "Rate limits and coalescing", "Family": "shaping", "Modes": modes})
	})
	a.mux.HandleFunc("GET /showcase/delivery/shaping/events", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if shapingModes[mode] == nil {
			http.Error(w, "Unknown mode", http.StatusBadRequest)
			return
		}
		a.serveOwned(w, r, ownedStream{
			reg: &l.reg, open: l.opener(mode), slow: shSlow, onRead: l.onRead, ended: l.ended,
			frame: func(m tavern.TopicMessage) string { return tavern.NewSSEMessage("value", m.Data).String() },
		})
	})
	a.mux.HandleFunc("POST /showcase/delivery/shaping/burst", func(w http.ResponseWriter, _ *http.Request) {
		if !l.burst() {
			http.Error(w, "A burst is already running", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	a.mux.HandleFunc("GET /showcase/delivery/shaping/state", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		writeJSON(w, map[string]any{
			"value": l.value, "produced": l.produced, "running": l.running.Load(),
			"written": maps.Clone(l.written), "brokerDrops": l.broker.PublishDrops(),
		})
	})
}

var shapingPage = deliveryPage("shaping", `{{define "main"}}
<p class="note">A burst publishes 50 values in about a quarter of a second. Each panel below is a slow window (every write waits 60 ms) with its own subscription. The plain one queues what it can and drops the rest, so it can miss the final value. The rate-limited one gets at most one value every 200 ms, always the latest. The coalescing one gets the latest value whenever it is ready for one. Replaced values are not counted as drops.</p>
<div class="panels">
{{range .Modes}}
  <section class="shape-panel" data-mode="{{.}}" sse-connect="/showcase/delivery/shaping/events?mode={{.}}&amp;conn={{$.Conn}}-{{.}}">
    <h2>{{.}}</h2>
    <p>Value: <output id="shape-{{.}}" sse-swap="value">…</output></p>
    <p>Updates received: <span data-received>0</span></p>
  </section>
{{end}}
</div>
<p>
  <button hx-post="/showcase/delivery/shaping/burst" hx-swap="none">Publish 50 values</button>
  <a href="/showcase/delivery/shaping?named=1">The named forms (per-second rate, team rate, team coalescing)</a>
  · <a href="/showcase/delivery/shaping?named=2">metadata and filters (every tenth value)</a>
</p>
<script>
document.body.addEventListener("htmx:sseMessage", function (e) {
  var panel = e.target.closest(".shape-panel");
  if (!panel || e.detail.type !== "value") return;
  var n = panel.querySelector("[data-received]");
  n.textContent = Number(n.textContent) + 1;
});
</script>
{{end}}`)
