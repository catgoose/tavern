package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The shutdown page owns a child broker, separate from the rest of the
// app: a publisher on it, and streams that are live, held back or slow.
// Shutting it down stops admitting streams, cancels the publisher, releases
// held writers, closes the broker (which ends every subscription and waits
// for the publisher) and reports what was left. A new child can then be
// started; a request that belonged to the old one never touches it.
const (
	chTopic    = "ops:child"
	chTick     = 500 * time.Millisecond
	chSlow     = 300 * time.Millisecond
	chDrainMax = 2 * time.Second
	chRetry    = 4 * time.Second // reconnect delay sent before shutting down
)

// child is one generation of the child broker.
type child struct {
	gen       int
	broker    *tavern.SSEBroker
	ctx       context.Context
	cancel    context.CancelFunc
	reg       connRegistry
	accepting atomic.Bool
	ticks     atomic.Int64
	published chan struct{} // closed when the publisher returns
}

type childLab struct {
	o *ops

	mu     sync.Mutex
	cur    *child
	gen    int
	report map[string]any
	log    obsLog
}

func newChildLab(o *ops) *childLab {
	l := &childLab{o: o}
	l.start()
	return l
}

// start creates a new child if none is running.
func (l *childLab) start() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil {
		return false
	}
	l.gen++
	c := &child{gen: l.gen, broker: tavern.NewSSEBroker(tavern.WithBufferSize(4)), published: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(l.o.ctx)
	c.accepting.Store(true)
	c.broker.RunPublisher(c.ctx, func(ctx context.Context) {
		defer close(c.published)
		for sleepCtx(ctx, chTick) {
			n := c.ticks.Add(1)
			c.broker.Publish(chTopic, tavern.NewSSEMessage("tick", fmt.Sprintf("child %d, tick %d", c.gen, n)).String())
		}
	})
	l.cur = c
	l.log.add("child %d started", c.gen)
	return true
}

// shutdown stops the current child and records what it found and left.
func (l *childLab) shutdown() map[string]any {
	l.mu.Lock()
	c := l.cur
	l.cur = nil
	l.mu.Unlock()
	if c == nil {
		return nil
	}
	start := time.Now()
	c.accepting.Store(false)
	before := c.reg.openStreams()
	c.broker.SetRetry(chTopic, chRetry) // ask the browsers to wait before reconnecting
	c.cancel()
	<-c.published
	c.reg.each(func(_ string, oc *ownedConn) { oc.gate.set(false) })
	c.broker.Close()
	deadline := time.Now().Add(chDrainMax)
	for c.reg.openStreams() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	report := map[string]any{
		"generation":        c.gen,
		"streamsBefore":     before,
		"streamsLeft":       c.reg.openStreams(),
		"subscriptionsLeft": c.broker.SubscriberCount(),
		"ticksPublished":    c.ticks.Load(),
		"took":              time.Since(start).Round(time.Millisecond).String(),
		"retryHint":         chRetry.String(),
	}
	l.mu.Lock()
	l.report = report
	l.mu.Unlock()
	l.log.add("child %d shut down: %d streams ended, %d left, %d subscriptions left", c.gen, before, report["streamsLeft"], report["subscriptionsLeft"])
	return report
}

var errChildDown = errors.New("the child broker is shut down")

func (l *childLab) current() *child {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

func (a *App) registerChild() {
	l := a.ops.child
	a.mux.HandleFunc("GET /showcase/ops/child", func(w http.ResponseWriter, _ *http.Request) {
		a.renderOpsPage(w, childPage, map[string]any{"Title": "Shutting down a broker", "Family": "child", "Modes": []string{"live", "held", "slow"}})
	})
	a.mux.HandleFunc("GET /showcase/ops/child/events", func(w http.ResponseWriter, r *http.Request) {
		c := l.current()
		if c == nil || !c.accepting.Load() {
			http.Error(w, errChildDown.Error(), http.StatusServiceUnavailable)
			return
		}
		mode := r.URL.Query().Get("mode")
		s := ownedStream{
			reg: &c.reg,
			open: func(conn string) (<-chan tavern.TopicMessage, func(), error) {
				if !c.accepting.Load() {
					return nil, nil, errChildDown
				}
				return openAll(func() (keyedSub, error) {
					ch, unsub := c.broker.SubscribeWith(chTopic, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn, Meta: map[string]string{"mode": mode}}))
					return subOrErr(mode, ch, unsub)
				})
			},
		}
		switch mode {
		case "live":
		case "slow":
			s.slow = chSlow
		case "held":
			s.open = holdFirst(&c.reg, s.open)
		default:
			http.Error(w, "Unknown mode", http.StatusBadRequest)
			return
		}
		a.serveOwned(w, r, s)
	})
	a.mux.HandleFunc("POST /showcase/ops/child/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		if l.shutdown() == nil {
			http.Error(w, "No child is running", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/ops/child/start", func(w http.ResponseWriter, _ *http.Request) {
		if !l.start() {
			http.Error(w, "A child is already running", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/ops/child/state", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{"running": false, "observations": l.log.list()}
		if c := l.current(); c != nil {
			streams := map[string]int{}
			for _, s := range c.broker.Subscribers(chTopic) {
				streams[s.Meta["mode"]]++
			}
			out["running"], out["generation"], out["streams"], out["ticks"] = true, c.gen, streams, c.ticks.Load()
		}
		l.mu.Lock()
		out["lastShutdown"] = l.report
		l.mu.Unlock()
		writeJSON(w, out)
	})
}

// holdFirst opens a stream whose writes are held back from the start.
func holdFirst(reg *connRegistry, open func(string) (<-chan tavern.TopicMessage, func(), error)) func(string) (<-chan tavern.TopicMessage, func(), error) {
	return func(conn string) (<-chan tavern.TopicMessage, func(), error) {
		ch, unsub, err := open(conn)
		if err == nil {
			if c := reg.owner(conn); c != nil {
				c.gate.set(true)
			}
		}
		return ch, unsub, err
	}
}

var childPage = deliveryPage("child", `{{define "main"}}
<p class="note">The child broker belongs to this page, not to the server. A publisher on it ticks every 500 ms. Open three streams on it: one live, one held back (its writes wait, so it does not even send its response headers) and one slow (each write waits 300 ms). Shutting it down stops new streams, sends the streams a 4 s reconnect delay (SetRetry), cancels the publisher, releases the held stream, closes the broker and waits for the streams to end; the report shows what was left. While it is down its streams are refused. Starting a new child makes a fresh broker for the next streams.</p>
<table id="child-streams">
  <tr><th>Stream</th><th>State</th><th>Last tick</th></tr>
  {{range .Modes}}<tr data-mode="{{.}}"><td>{{.}}</td><td data-state>not open</td><td data-tick></td></tr>
  {{end}}
</table>
<p>
  <button type="button" id="child-open">Open the streams</button>
  <button hx-post="/showcase/ops/child/shutdown" hx-swap="none">Shut the child down</button>
  <button hx-post="/showcase/ops/child/start" hx-swap="none">Start a new child</button>
</p>
<script>
(function () {
  // Plain EventSources, closed on their first error, so a stream the
  // child refuses is not retried.
  var open = {};
  function row(mode, sel) { return document.querySelector('[data-mode="' + mode + '"] ' + sel); }
  document.getElementById("child-open").addEventListener("click", function () {
    ["live", "held", "slow"].forEach(function (mode) {
      if (open[mode]) return;
      var es = new EventSource("/showcase/ops/child/events?mode=" + mode + "&conn={{.Conn}}-" + mode);
      open[mode] = es;
      row(mode, "[data-state]").textContent = "connecting";
      es.onopen = function () { row(mode, "[data-state]").textContent = "open"; };
      es.addEventListener("tick", function (e) { row(mode, "[data-tick]").textContent = e.data; });
      es.onerror = function () {
        es.close();
        delete open[mode];
        row(mode, "[data-state]").textContent = "ended";
      };
    });
  });
})();
</script>
{{end}}`)
