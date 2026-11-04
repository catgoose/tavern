package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The pressure family uses adaptive backpressure on small queues. Tiers
// follow a window's consecutive drops: at 2 it is throttled (every second
// message skipped), at 4 it gets simplified content, at 8 it is evicted. A
// successful send resets it to normal. The current value is republished
// after a drop until it is delivered, so a window that was held back ends
// on the current value with no further change.
const (
	prTopic   = "delivery/gauge"
	prWait    = 300 * time.Millisecond // PublishBlocking timeout
	prMaxStep = 30
)

var prThresholds = tavern.AdaptiveBackpressure{ThrottleAt: 2, SimplifyAt: 4, DisconnectAt: 8}

type pressureLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry
	repair feedRepair

	pubMu   sync.Mutex
	mu      sync.Mutex
	value   int
	results []string // blocking publish results, newest first
	log     obsLog

	// holdRepair, with test controls, makes the repair wait before it
	// republishes; stop releases it when the lab stops.
	holdRepair *pauser
	stop       context.Context
}

func newPressureLab(d *delivery) *pressureLab {
	l := &pressureLab{broker: d.broker(tavern.WithBufferSize(4), tavern.WithAdaptiveBackpressure(prThresholds)), stop: d.ctx}
	l.broker.SetSimplifiedRenderer(prTopic, simplifyGauge)
	l.broker.OnBackpressureTierChange(func(sub *tavern.SubscriberInfo, oldTier, newTier tavern.BackpressureTier) {
		l.log.add("%s: %s → %s", sub.ID, oldTier, newTier)
	})
	l.repair.init(func(topic string) bool { return topic == prTopic }, l.republish)
	l.broker.OnPublishDrop(func(topic string, _ int) { l.repair.onDrop(topic) })
	return l
}

// gaugeHTML is the full representation of a value.
func gaugeHTML(v int) string {
	return fmt.Sprintf(`<div data-value="%d" data-fidelity="full"><output>%d</output>`+
		`<table><tr><th>Reading</th><td>%d</td></tr><tr><th>Trend</th><td>%s</td></tr><tr><th>Band</th><td>%s</td></tr></table></div>`,
		v, v, v, trend(v), band(v))
}

func trend(v int) string {
	if v%2 == 0 {
		return "rising"
	}
	return "steady"
}

func band(v int) string {
	switch {
	case v%30 < 10:
		return "low"
	case v%30 < 20:
		return "middle"
	}
	return "high"
}

var gaugeValue = regexp.MustCompile(`data-value="(\d+)"`)

// simplifyGauge keeps the value and drops the detail. Messages without a
// value (none are published here) pass through unchanged.
func simplifyGauge(msg string) string {
	m := gaugeValue.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	return tavern.RenderFragments(tavern.Replace("gauge",
		fmt.Sprintf(`<div data-value=%q data-fidelity="simplified"><output>%s</output> (simplified)</div>`, m[1], m[1])))
}

func (l *pressureLab) message() string {
	return tavern.RenderFragments(tavern.Replace("gauge", gaugeHTML(l.value)))
}

// change makes n changes.
func (l *pressureLab) change(n int) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	for range n {
		l.mu.Lock()
		l.value++
		msg := l.message()
		l.mu.Unlock()
		l.broker.Publish(prTopic, msg)
	}
}

// republish sends the current value again, to everyone and to the team.
func (l *pressureLab) republish() {
	if l.holdRepair != nil {
		l.holdRepair.wait(l.stop, nil)
	}
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	msg := l.message()
	l.mu.Unlock()
	l.broker.Publish(prTopic, msg)
	l.broker.PublishTo(prTopic, deliveryTeam, msg)
}

// blocking makes one change and publishes it with PublishBlocking (or, for
// the team, PublishBlockingTo), waiting up to prWait for a full queue. A
// timeout is not counted as a drop, so the repair is started here.
func (l *pressureLab) blocking(team bool) error {
	l.pubMu.Lock()
	l.mu.Lock()
	l.value++
	msg := l.message()
	l.mu.Unlock()
	start := time.Now()
	var err error
	if team {
		err = l.broker.PublishBlockingTo(prTopic, deliveryTeam, msg, prWait)
	} else {
		err = l.broker.PublishBlocking(prTopic, msg, prWait)
	}
	l.pubMu.Unlock()
	who := "everyone"
	if team {
		who = "the red team"
	}
	result := fmt.Sprintf("to %s: delivered in %s", who, time.Since(start).Round(time.Millisecond))
	if errors.Is(err, tavern.ErrPublishTimeout) {
		result = fmt.Sprintf("to %s: %v after %s", who, err, time.Since(start).Round(time.Millisecond))
		l.repair.onDrop(prTopic)
	}
	l.mu.Lock()
	l.results = append([]string{result}, l.results...)
	if len(l.results) > deliveryLogLimit {
		l.results = l.results[:deliveryLogLimit]
	}
	l.mu.Unlock()
	return err
}

func (l *pressureLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	snap := tavern.NewSSEMessage("oob", l.message()).String()
	l.mu.Unlock()
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	return openAll(
		func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(prTopic, meta, tavern.SubWithSnapshot(func() string { return snap }))
			return subOrErr("everyone", ch, unsub)
		},
		func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(prTopic, meta, tavern.SubWithScope(deliveryTeam))
			return subOrErr("team", ch, unsub)
		},
	)
}

func (a *App) registerPressure() {
	l := a.delivery.pressure
	a.mux.HandleFunc("GET /showcase/delivery/pressure", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, pressurePage, map[string]any{"Title": "Adaptive backpressure", "Family": "pressure", "TavernJS": true})
	})
	a.mux.HandleFunc("GET /showcase/delivery/pressure/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open})
	})
	a.mux.HandleFunc("POST /showcase/delivery/pressure/pause", pauseOwned(&l.reg))
	a.mux.HandleFunc("POST /showcase/delivery/pressure/change", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 1 || n > prMaxStep {
			http.Error(w, "n must be from 1 to 30", http.StatusBadRequest)
			return
		}
		l.change(n)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/delivery/pressure/blocking", func(w http.ResponseWriter, r *http.Request) {
		_ = l.blocking(r.URL.Query().Get("team") == "1") // the result is listed in the state
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/delivery/pressure/state", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		writeJSON(w, map[string]any{
			"value": l.value, "blocking": append([]string{}, l.results...), "tiers": l.log.list(),
			"brokerDrops": l.broker.PublishDrops(), "repairPending": l.repair.pending(),
		})
	})
}

var pressurePage = deliveryPage("pressure", `{{define "main"}}
<p class="note">Queues here hold four messages. Hold this window back, make changes, and release it. After two drops in a row the window is throttled, after four it gets simplified content (the value without its detail), and after eight it is evicted and reconnects to a snapshot. A successful send puts it back to normal. Tier changes are sent as <code>tavern-backpressure</code> events, which tavern-js reports as <code>tavern:stream-degraded</code> and <code>tavern:stream-restored</code>. They are sent without waiting, so one that finds the queue full is dropped; the server's list shows every change. After a drop the server republishes the current value until it is delivered.</p>
<section id="pressure-region" sse-connect="/showcase/delivery/pressure/events?conn={{.Conn}}">
  <div sse-swap="oob,tavern-backpressure" hx-swap="none"></div>
  <div id="gauge">…</div>
</section>
<p>
  <button hx-post="/showcase/delivery/pressure/pause?conn={{.Conn}}&amp;on=1" hx-swap="none">Hold back this window</button>
  <button hx-post="/showcase/delivery/pressure/pause?conn={{.Conn}}&amp;on=0" hx-swap="none">Release it</button>
  <button hx-post="/showcase/delivery/pressure/change?n=6" hx-swap="none">Make 6 changes</button>
  <button hx-post="/showcase/delivery/pressure/change?n=10" hx-swap="none">Make 10 changes</button>
  <button hx-post="/showcase/delivery/pressure/change?n=30" hx-swap="none">Make 30 changes</button>
</p>
<p>
  <button hx-post="/showcase/delivery/pressure/blocking" hx-swap="none">Publish a change and wait up to 300 ms</button>
  <button hx-post="/showcase/delivery/pressure/blocking?team=1" hx-swap="none">The same, for the red team</button>
</p>
{{end}}`)
