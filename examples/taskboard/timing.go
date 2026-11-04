package main

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The timing family publishes one burst of changes three ways: at once,
// debounced (only after a quiet period) and throttled (the first at once,
// then at most one per interval, latest wins).
const (
	tmNow       = "delivery/timing/now"
	tmDebounced = "delivery/timing/debounced"
	tmThrottled = "delivery/timing/throttled"
	tmDebounce  = 300 * time.Millisecond
	tmThrottle  = 250 * time.Millisecond
	tmSpacing   = 20 * time.Millisecond
	tmBurstMax  = 40
)

var timingTopics = []string{tmNow, tmDebounced, tmThrottled}

type timingLab struct {
	d      *delivery
	broker *tavern.SSEBroker
	reg    connRegistry

	pubMu     sync.Mutex
	mu        sync.Mutex
	value     int
	produced  int
	published map[string]*atomic.Int64 // topic → publishes that reached the broker
	running   atomic.Bool
}

func newTimingLab(d *delivery) *timingLab {
	l := &timingLab{d: d, broker: d.broker(tavern.WithBufferSize(64)), published: map[string]*atomic.Int64{}}
	for _, t := range timingTopics {
		n := &atomic.Int64{}
		l.published[t] = n
		l.broker.After(t, func() { n.Add(1) })
	}
	return l
}

func timingElement(topic string) string {
	switch topic {
	case tmDebounced:
		return "timing-debounced"
	case tmThrottled:
		return "timing-throttled"
	}
	return "timing-now"
}

func timingFrag(topic string, v int) string {
	return tavern.RenderFragments(tavern.Replace(timingElement(topic), `<output data-value="`+strconv.Itoa(v)+`">`+strconv.Itoa(v)+`</output>`))
}

// step makes one change and hands it to all three publishers. Debounced and
// throttled publishes are latest-wins per topic, and every change goes
// through the same calls, so a delayed publish never carries an older value
// than one already published on its topic.
func (l *timingLab) step() {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	l.value++
	l.produced++
	v := l.value
	l.mu.Unlock()
	l.broker.Publish(tmNow, timingFrag(tmNow, v))
	l.broker.PublishDebounced(tmDebounced, timingFrag(tmDebounced, v), tmDebounce)
	l.broker.PublishThrottled(tmThrottled, timingFrag(tmThrottled, v), tmThrottle)
}

// burst makes n changes, tmSpacing apart, in the background. Only one burst
// runs at a time.
func (l *timingLab) burst(n int) bool {
	if !l.running.CompareAndSwap(false, true) {
		return false
	}
	if !l.d.goBounded(func(ctx context.Context) {
		defer l.running.Store(false)
		for i := range n {
			if i > 0 && !sleepCtx(ctx, tmSpacing) {
				return
			}
			l.step()
		}
	}) {
		l.running.Store(false) // the lab is stopping
		return false
	}
	return true
}

func (l *timingLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	v := l.value
	l.mu.Unlock()
	snap := tavern.NewSSEMessage("oob", timingFrag(tmNow, v)+timingFrag(tmDebounced, v)+timingFrag(tmThrottled, v)).String()
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	var opens []func() (keyedSub, error)
	for i, t := range timingTopics {
		opts := []tavern.SubscribeOption{meta}
		if i == 0 {
			opts = append(opts, tavern.SubWithSnapshot(func() string { return snap }))
		}
		opens = append(opens, func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(t, opts...)
			return subOrErr(t, ch, unsub)
		})
	}
	return openAll(opens...)
}

func (l *timingLab) state() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	pub := map[string]int64{}
	for t, n := range l.published {
		pub[timingElement(t)] = n.Load()
	}
	return map[string]any{"value": l.value, "produced": l.produced, "published": pub, "running": l.running.Load()}
}

func (a *App) registerTiming() {
	l := a.delivery.timing
	a.mux.HandleFunc("GET /showcase/delivery/timing", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, timingPage, map[string]any{"Title": "Immediate, debounced and throttled", "Family": "timing"})
	})
	a.mux.HandleFunc("GET /showcase/delivery/timing/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open})
	})
	a.mux.HandleFunc("POST /showcase/delivery/timing/burst", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 1 || n > tmBurstMax {
			http.Error(w, "n must be from 1 to 40", http.StatusBadRequest)
			return
		}
		if !l.burst(n) {
			http.Error(w, "A burst is already running", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	a.mux.HandleFunc("GET /showcase/delivery/timing/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, l.state())
	})
}

var timingPage = deliveryPage("timing", `{{define "main"}}
<p class="note">A burst makes twenty changes, 20 ms apart. Each change is published at once, debounced (300 ms of quiet) and throttled (the first at once, then at most one every 250 ms, latest wins). Once the burst is over all three show the same final value; they differ in how many updates it took.</p>
<section id="timing-region" sse-connect="/showcase/delivery/timing/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <table>
    <tr><th></th><th>Value</th><th>Updates received</th></tr>
    <tr><th>Immediate</th><td id="timing-now">…</td><td data-count-for="timing-now">0</td></tr>
    <tr><th>Debounced</th><td id="timing-debounced">…</td><td data-count-for="timing-debounced">0</td></tr>
    <tr><th>Throttled</th><td id="timing-throttled">…</td><td data-count-for="timing-throttled">0</td></tr>
  </table>
</section>
<p><button hx-post="/showcase/delivery/timing/burst?n=20" hx-swap="none">Make 20 changes</button></p>
{{end}}`)
