package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The scheduled family renders two dashboard sections on their own
// intervals with a ScheduledPublisher. Nothing is rendered while no window
// is subscribed. The load section has a circuit breaker: after two failed
// renders in a row it shows a fallback, and after the recovery interval it
// tries again.
const (
	scTopic    = "delivery/dashboard"
	scTick     = 50 * time.Millisecond
	scClock    = time.Second
	scClockAlt = 300 * time.Millisecond
	scLoad     = 400 * time.Millisecond
	scRecover  = 1500 * time.Millisecond
)

var errLoadDown = errors.New("load source unavailable")

type schedLab struct {
	broker *tavern.SSEBroker
	pub    *tavern.ScheduledPublisher
	reg    connRegistry

	fail      atomic.Bool
	mu        sync.Mutex
	interval  time.Duration // the clock's
	attempts  map[string]int
	successes map[string]int
	errors    int
	fallbacks int
	published atomic.Int64
	log       obsLog
}

func newSchedLab(d *delivery) *schedLab {
	l := &schedLab{
		broker:    d.broker(tavern.WithBufferSize(16)),
		interval:  scClock,
		attempts:  map[string]int{},
		successes: map[string]int{},
	}
	l.pub = l.broker.NewScheduledPublisher(scTopic, tavern.WithBaseTick(scTick))
	l.pub.Register("clock", scClock, l.renderClock)
	l.pub.Register("load", scLoad, l.renderLoad, tavern.SectionOptions{CircuitBreaker: &tavern.CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryInterval: scRecover,
		FallbackRender:   l.fallback,
	}})
	// Runs synchronously in the publisher's goroutine: it only records.
	l.broker.OnRenderError(func(e *tavern.RenderError) {
		l.mu.Lock()
		l.errors++
		l.mu.Unlock()
		l.log.add("section %s failed (%d in a row): %v", e.Section, e.Count, e.Err)
	})
	l.broker.After(scTopic, func() { l.published.Add(1) })
	l.broker.RunPublisher(d.ctx, l.pub.Start)
	return l
}

func (l *schedLab) note(section string, ok bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[section]++
	if ok {
		l.successes[section]++
	}
	return l.attempts[section]
}

func (l *schedLab) renderClock(_ context.Context, buf *bytes.Buffer) error {
	n := l.note("clock", true)
	buf.WriteString(tavern.RenderFragments(tavern.Replace("dash-clock", fmt.Sprintf(`<span data-renders="%d">%s</span> (render %d)`, n, time.Now().Format("15:04:05.0"), n))))
	return nil
}

func (l *schedLab) renderLoad(_ context.Context, buf *bytes.Buffer) error {
	if l.fail.Load() {
		l.note("load", false)
		return errLoadDown
	}
	n := l.note("load", true)
	buf.WriteString(tavern.RenderFragments(tavern.Replace("dash-load", fmt.Sprintf(`<span data-state="live">%d%% (render %d)</span>`, 20+n%60, n))))
	return nil
}

func (l *schedLab) fallback() string {
	l.mu.Lock()
	l.fallbacks++
	n := l.fallbacks
	l.mu.Unlock()
	return tavern.RenderFragments(tavern.Replace("dash-load", fmt.Sprintf(`<span data-state="fallback">Unavailable; showing the fallback (%d)</span>`, n)))
}

func (l *schedLab) setFast(fast bool) {
	d := scClock
	if fast {
		d = scClockAlt
	}
	l.pub.SetInterval("clock", d)
	l.mu.Lock()
	l.interval = d
	l.mu.Unlock()
	l.log.add("clock interval set to %s", d)
}

func (l *schedLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	return openAll(func() (keyedSub, error) {
		ch, unsub := l.broker.SubscribeWith(scTopic, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn}))
		return subOrErr(scTopic, ch, unsub)
	})
}

func (l *schedLab) state() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{
		"attempts": maps.Clone(l.attempts), "successes": maps.Clone(l.successes), "errors": l.errors,
		"fallbacks": l.fallbacks, "published": l.published.Load(), "failing": l.fail.Load(),
		"clockInterval": l.interval.String(), "subscribers": l.broker.HasSubscribers(scTopic),
		"observations": l.log.list(),
	}
}

func (a *App) registerSched() {
	l := a.delivery.sched
	a.mux.HandleFunc("GET /showcase/delivery/sched", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, schedPage, map[string]any{"Title": "Scheduled sections", "Family": "sched"})
	})
	a.mux.HandleFunc("GET /showcase/delivery/sched/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open})
	})
	a.mux.HandleFunc("POST /showcase/delivery/sched/fail", func(w http.ResponseWriter, r *http.Request) {
		on := r.URL.Query().Get("on") == "1"
		l.fail.Store(on)
		l.log.add("load source failing: %t", on)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/delivery/sched/interval", func(w http.ResponseWriter, r *http.Request) {
		l.setFast(r.URL.Query().Get("fast") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/delivery/sched/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, l.state())
	})
}

var schedPage = deliveryPage("sched", `{{define "main"}}
<p class="note">A scheduled publisher renders the clock every second and the load every 400 ms, and only while some window is subscribed. Make the load source fail: after two failures in a row its circuit opens and the fallback is shown. After 1.5 s it tries the source again, and once the source works it goes back to live readings. The failure setting is shared by every window.</p>
<section id="sched-region" sse-connect="/showcase/delivery/sched/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <p>Clock: <span id="dash-clock">…</span></p>
  <p>Load: <span id="dash-load">…</span></p>
</section>
<p>
  <button hx-post="/showcase/delivery/sched/fail?on=1" hx-swap="none">Make the load source fail</button>
  <button hx-post="/showcase/delivery/sched/fail?on=0" hx-swap="none">Repair it</button>
  <button hx-post="/showcase/delivery/sched/interval?fast=1" hx-swap="none">Clock every 300 ms</button>
  <button hx-post="/showcase/delivery/sched/interval?fast=0" hx-swap="none">Clock every second</button>
</p>
{{end}}`)
