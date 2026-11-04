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
// Each request is admitted by one generation and ends with it. Shutting a
// generation down is drain; a new child can then be started, and a request
// that belonged to the old one never touches it. When the app shuts down
// the lab is closed for good and no child starts again.
const (
	chTopic    = "ops:child"
	chTick     = 500 * time.Millisecond
	chSlow     = 300 * time.Millisecond
	chDrainMax = 2 * time.Second
	chGrace    = 500 * time.Millisecond // for streams to write what is queued, such as the retry
	chRetry    = 4 * time.Second        // reconnect delay sent before shutting down
)

// child is one generation of the child broker.
type child struct {
	gen       int
	broker    *tavern.SSEBroker
	ctx       context.Context // ends every request of this generation
	cancel    context.CancelFunc
	pubCancel context.CancelFunc // stops the publisher
	reg       connRegistry
	ticks     atomic.Int64
	published chan struct{} // closed when the publisher returns

	// mu guards admission. A request is admitted only while accepting, and
	// counted until its handler returns; shutdown stops admission before it
	// waits, so no request can join a generation that is draining.
	mu        sync.Mutex
	accepting bool
	active    int
	handlers  sync.WaitGroup
}

// enter admits one request for the lifetime of its handler.
func (c *child) enter() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting {
		return false
	}
	c.active++
	c.handlers.Add(1)
	return true
}

func (c *child) exit() {
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	c.handlers.Done()
}

func (c *child) isAccepting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepting
}

// seal stops admission and returns the requests still running.
func (c *child) seal() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accepting = false
	return c.active
}

func (c *child) running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

type childLab struct {
	o *ops

	mu       sync.Mutex
	cur      *child
	gen      int
	closed   bool           // the app is shutting down: no child can start again
	draining sync.WaitGroup // generations being shut down
	retired  int
	report   map[string]any
	log      obsLog

	// opened, when set, is called by a held stream after it subscribes and
	// before it holds its writes; tests use it to shut down in between.
	opened func(conn string)
}

var (
	errChildDown    = errors.New("the child broker is shut down")
	errChildRunning = errors.New("a child is already running")
	errLabClosed    = errors.New("the operations lab is shutting down")
)

func newChildLab(o *ops) *childLab {
	l := &childLab{o: o}
	_ = l.start()
	return l
}

// start creates a new child if none is running and the lab is open.
func (l *childLab) start() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.closed:
		return errLabClosed
	case l.cur != nil:
		return errChildRunning
	}
	l.gen++
	c := &child{gen: l.gen, broker: tavern.NewSSEBroker(tavern.WithBufferSize(4)), published: make(chan struct{}), accepting: true}
	c.ctx, c.cancel = context.WithCancel(l.o.ctx)
	var pubCtx context.Context
	pubCtx, c.pubCancel = context.WithCancel(c.ctx)
	c.broker.RunPublisher(pubCtx, func(ctx context.Context) {
		defer close(c.published)
		for sleepCtx(ctx, chTick) {
			n := c.ticks.Add(1)
			c.broker.Publish(chTopic, tavern.NewSSEMessage("tick", fmt.Sprintf("child %d, tick %d", c.gen, n)).String())
		}
	})
	l.cur = c
	l.log.add("child %d started", c.gen)
	return nil
}

// shutdown stops the current child, which the app can start again.
func (l *childLab) shutdown() map[string]any {
	l.mu.Lock()
	c := l.cur
	l.cur = nil
	if c != nil {
		l.draining.Add(1)
	}
	l.mu.Unlock()
	if c == nil {
		return nil
	}
	defer l.draining.Done()
	return l.drain(c)
}

// close shuts the lab's child down for good: no child starts after it, and
// it returns once every generation, including one another caller is still
// shutting down, has drained.
func (l *childLab) close() {
	l.mu.Lock()
	l.closed = true
	c := l.cur
	l.cur = nil
	if c != nil {
		l.draining.Add(1)
	}
	l.mu.Unlock()
	if c != nil {
		l.drain(c)
		l.draining.Done()
	}
	l.draining.Wait()
}

// drain stops admission, asks the streams to wait before reconnecting,
// stops the publisher, releases held writers and closes the broker, so
// established streams write what is queued and end. After chGrace it
// cancels the generation's requests, ending any stream that was still
// opening or is stuck writing, and waits at most chDrainMax in all for
// the handlers to return.
func (l *childLab) drain(c *child) map[string]any {
	start := time.Now()
	before := c.seal()
	c.broker.SetRetry(chTopic, chRetry)
	c.pubCancel()
	<-c.published
	c.reg.each(func(_ string, oc *ownedConn) { oc.gate.set(false) })
	c.broker.Close()
	done := make(chan struct{})
	go func() { c.handlers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(chGrace):
		c.cancel()
		select {
		case <-done:
		case <-time.After(chDrainMax - chGrace):
		}
	}
	c.cancel()
	report := map[string]any{
		"generation":        c.gen,
		"streamsBefore":     before,
		"streamsLeft":       c.running(),
		"subscriptionsLeft": c.broker.SubscriberCount(),
		"ticksPublished":    c.ticks.Load(),
		"took":              time.Since(start).Round(time.Millisecond).String(),
		"retryHint":         chRetry.String(),
	}
	l.mu.Lock()
	l.report = report
	l.retired++
	l.mu.Unlock()
	l.log.add("child %d shut down: %d streams ended, %d left, %d subscriptions left", c.gen, before, report["streamsLeft"], report["subscriptionsLeft"])
	return report
}

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
		if c == nil || !c.enter() {
			http.Error(w, errChildDown.Error(), http.StatusServiceUnavailable)
			return
		}
		defer c.exit()
		// The request ends with its generation, so a stream still opening
		// when the child shuts down cannot outlive it.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(c.ctx, cancel)()
		r = r.WithContext(ctx)
		mode := r.URL.Query().Get("mode")
		s := ownedStream{
			reg: &c.reg,
			open: func(conn string) (<-chan tavern.TopicMessage, func(), error) {
				if !c.isAccepting() {
					return nil, nil, errChildDown
				}
				return openAll(func() (keyedSub, error) {
					ch, unsub := c.broker.SubscribeWith(chTopic, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn, Meta: map[string]string{"mode": mode}}))
					if ch != nil && mode == "held" && l.opened != nil {
						l.opened(conn)
					}
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
		switch err := l.start(); {
		case errors.Is(err, errLabClosed):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusConflict)
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
<p class="note">The child broker belongs to this page, not to the server. A publisher on it ticks every 500 ms. Open three streams on it: one live, one held back (its writes wait, so it does not even send its response headers) and one slow (each write waits 300 ms). Shutting it down stops new streams, sends the streams a 4 s reconnect delay (SetRetry), stops the publisher, releases the held stream and closes the broker; streams that have not ended half a second later, such as one still opening, are cancelled. The report shows what was left. While it is down its streams are refused. Starting a new child makes a fresh broker for the next streams.</p>
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
