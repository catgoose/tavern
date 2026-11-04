package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The delivery lab shows how Tavern shapes delivery: change-only and lazy
// rendering, timing, expiry, batching and ordering, per-subscriber rate and
// coalescing, adaptive backpressure, reactive hooks and scheduled sections.
// Each family runs on its own broker, so broker-wide options and callbacks
// never reach the rest of the app.
const (
	deliveryLogLimit = 30
	deliveryTeam     = "red" // the scope used by the scoped variants
)

// labRuntime owns a lab's brokers and background work: the context that
// stops the work, and the group it waits for.
type labRuntime struct {
	ctx     context.Context
	cancel  context.CancelFunc
	brokers []*tavern.SSEBroker

	mu      sync.Mutex // guards stopped and additions to wg
	stopped bool
	wg      sync.WaitGroup
}

func newLabRuntime() labRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return labRuntime{ctx: ctx, cancel: cancel}
}

func (r *labRuntime) broker(opts ...tavern.BrokerOption) *tavern.SSEBroker {
	b := tavern.NewSSEBroker(opts...)
	r.brokers = append(r.brokers, b)
	return b
}

// goBounded runs fn in the background until it returns or the lab stops.
// Once the lab has begun stopping it runs nothing and returns false.
func (r *labRuntime) goBounded(fn func(ctx context.Context)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn(r.ctx)
	}()
	return true
}

// stop refuses new background work, cancels what is running and waits
// for it.
func (r *labRuntime) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

func (r *labRuntime) closeBrokers() {
	for _, b := range r.brokers {
		b.Close()
	}
}

// delivery holds the lab's families and their runtime.
type delivery struct {
	labRuntime

	change   *changeLab
	timing   *timingLab
	ttl      *ttlLab
	batch    *batchLab
	shaping  *shapingLab
	pressure *pressureLab
	flow     *flowLab
	sched    *schedLab
}

func newDelivery() *delivery {
	d := &delivery{labRuntime: newLabRuntime()}
	d.change = newChangeLab(d)
	d.timing = newTimingLab(d)
	d.ttl = newTTLLab(d)
	d.batch = newBatchLab(d)
	d.shaping = newShapingLab(d)
	d.pressure = newPressureLab(d)
	d.flow = newFlowLab(d)
	d.sched = newSchedLab(d)
	return d
}

func (d *delivery) close() {
	d.stop()
	d.pressure.repair.close()
	d.closeBrokers()
}

// sleepCtx waits for d or for ctx to end, reporting which.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// obsLog is a bounded, newest-first log of what the server saw.
type obsLog struct {
	mu  sync.Mutex
	log []string
}

func (l *obsLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.log = append([]string{time.Now().Format("15:04:05.000") + "  " + fmt.Sprintf(format, args...)}, l.log...)
	if len(l.log) > deliveryLogLimit {
		l.log = l.log[:deliveryLogLimit]
	}
}

func (l *obsLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.log...)
}

func (l *obsLog) has(text string) bool {
	for _, s := range l.list() {
		if strings.Contains(s, text) {
			return true
		}
	}
	return false
}

// keyedSub is one subscription feeding a window's stream.
type keyedSub struct {
	key   string
	ch    <-chan string
	unsub func()
}

// mergeSubs forwards every subscription into one channel, keyed by
// subscription. The merged stream ends when any subscription ends, so a
// window whose subscription was evicted reconnects as a whole.
func mergeSubs(subs ...keyedSub) (msgs <-chan tavern.TopicMessage, unsubscribe func()) {
	out := make(chan tavern.TopicMessage)
	stop := make(chan struct{})
	var once sync.Once
	end := func() { once.Do(func() { close(stop) }) }
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer end()
			for {
				select {
				case m, ok := <-s.ch:
					if !ok {
						return
					}
					select {
					case out <- tavern.TopicMessage{Topic: s.key, Data: m}:
					case <-stop:
						return
					}
				case <-stop:
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, func() {
		end()
		for _, s := range subs {
			s.unsub()
		}
		wg.Wait()
	}
}

// deliveryFrame turns a message into an SSE frame. Messages that are already
// frames pass through: events, Tavern's control events, retry directives
// (SetRetry) and comments. Raw OOB HTML is sent as an "oob" event, which the
// page swaps out of band.
func deliveryFrame(m tavern.TopicMessage) string {
	for _, prefix := range []string{"event: ", "data: ", "id: ", "retry: ", ":"} {
		if strings.HasPrefix(m.Data, prefix) {
			return m.Data
		}
	}
	return tavern.NewSSEMessage("oob", m.Data).String()
}

// ownedStream describes one window's stream: how to open its
// subscriptions, an optional delay before each write (a slow client), and
// an optional hook that sees each channel message.
type ownedStream struct {
	reg  *connRegistry
	open func(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error)
	// openSnapshot, when set, is used instead of open. Its snapshot is
	// written as the first frame, before any message from the merged
	// subscriptions; a per-subscription snapshot could be overtaken by
	// another subscription's live message.
	openSnapshot func(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), snapshot string, err error)
	slow         time.Duration
	frame        func(m tavern.TopicMessage) string // deliveryFrame when nil
	onRead       func(conn string, m tavern.TopicMessage)
	ended        func(conn string)
}

var errNoSubscription = errors.New("the broker refused the subscription")

// serveOwned streams one window's subscriptions. The connection ID is
// reserved before anything is subscribed, so a second request with the
// same ID is refused and cannot take over the first one's controls.
func (a *App) serveOwned(w http.ResponseWriter, r *http.Request, s ownedStream) {
	conn := r.URL.Query().Get("conn")
	if conn == "" {
		http.Error(w, "Missing connection", http.StatusBadRequest)
		return
	}
	c := newOwnedConn()
	if err := s.reg.reserve(conn, c); err != nil {
		code := http.StatusConflict
		if errors.Is(err, errConnsFull) {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), code)
		return
	}
	unsub := func() {}
	defer func() {
		unsub()
		c.gate.set(false)
		s.reg.release(conn, c)
		close(c.done)
		if s.ended != nil {
			s.ended(conn)
		}
	}()
	var ch <-chan tavern.TopicMessage
	var u func()
	var snapshot string
	var err error
	if s.openSnapshot != nil {
		ch, u, snapshot, err = s.openSnapshot(conn)
	} else {
		ch, u, err = s.open(conn)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	unsub = u
	s.reg.setUnsubscribe(c, unsub)
	pw := pausableWriter{w, func() {
		if s.slow > 0 {
			sleepCtx(r.Context(), s.slow)
		}
		c.gate.wait(r.Context(), a.done)
	}}
	encode := s.frame
	if encode == nil {
		encode = deliveryFrame
	}
	frame := encode
	if s.onRead != nil {
		frame = func(m tavern.TopicMessage) string {
			s.onRead(conn, m)
			return encode(m)
		}
	}
	err = tavern.StreamSSE(r.Context(), pw, ch, frame,
		tavern.WithStreamSnapshot(func() string { return snapshot }),
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	a.logger.Info("delivery stream ended", "path", r.URL.Path, "write_error", err)
}

// pauseOwned holds back or releases a window's writes.
func pauseOwned(reg *connRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := reg.owner(r.URL.Query().Get("conn"))
		if c == nil {
			http.Error(w, "This window has no stream", http.StatusNotFound)
			return
		}
		c.gate.set(r.URL.Query().Get("on") == "1")
		w.WriteHeader(http.StatusNoContent)
	}
}

// subOrErr turns a refused subscription (nil channel) into an error.
func subOrErr(key string, ch <-chan string, unsub func()) (keyedSub, error) {
	if ch == nil {
		return keyedSub{}, errNoSubscription
	}
	return keyedSub{key: key, ch: ch, unsub: unsub}, nil
}

// openAll opens each subscription in turn, undoing the earlier ones if a
// later one is refused, and merges them.
func openAll(opens ...func() (keyedSub, error)) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	var subs []keyedSub
	for _, open := range opens {
		s, err := open()
		if err != nil {
			for _, done := range subs {
				done.unsub()
			}
			return nil, nil, err
		}
		subs = append(subs, s)
	}
	ch, unsub := mergeSubs(subs...)
	return ch, unsub, nil
}

func (a *App) registerDelivery() {
	a.delivery = newDelivery()
	a.mux.HandleFunc("GET /showcase/delivery", a.deliveryIndex)
	a.registerChange()
	a.registerTiming()
	a.registerTTL()
	a.registerBatch()
	a.registerShaping()
	a.registerPressure()
	a.registerFlow()
	a.registerSched()
}

type deliveryFamily struct{ Path, Title, About string }

var deliveryFamilies = []deliveryFamily{
	{"render", "Change-only and lazy rendering", "Publishing only what changed, rendering only for an audience, and server-rendered components."},
	{"timing", "Immediate, debounced and throttled", "One burst of changes published three ways."},
	{"ttl", "Expiring messages", "Live delivery is immediate; expiry removes messages from the replay cache, and optionally from the page."},
	{"batch", "Batches and ordering", "One channel message per topic and scope for a batch, and the same order for every subscriber of an ordered topic."},
	{"shaping", "Rate limits and coalescing", "A fast producer and slow windows, plain, rate-limited and coalescing."},
	{"pressure", "Adaptive backpressure", "Tiers that throttle, simplify and finally evict a window that cannot keep up, and blocking publishes."},
	{"flow", "Reactive workflow", "A mutation hook, a derived update, middleware and first/last subscriber hooks."},
	{"sched", "Scheduled sections", "Sections rendered on their own intervals, with a circuit breaker on one."},
}

func (a *App) deliveryIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := deliveryIndexTmpl.Execute(w, deliveryFamilies); err != nil {
		a.logger.Error("render delivery index", "err", err)
	}
}

// renderDeliveryPage renders one family's page with a new connection ID.
func (a *App) renderDeliveryPage(w http.ResponseWriter, t *template.Template, data map[string]any) {
	data["Conn"] = newConnID()
	if data["Base"] == nil {
		data["Base"], data["Lab"] = "/showcase/delivery", "Delivery lab"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		a.logger.Error("render delivery page", "err", err)
	}
}

// deliveryPage parses a family's page from the shared layout and the
// family's own "main" template.
func deliveryPage(name, main string) *template.Template {
	t := template.Must(template.New(name).Parse(deliveryLayoutHTML))
	return template.Must(t.Parse(main))
}

const deliveryIndexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Delivery lab</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header>
  <h1>Delivery lab</h1>
  <nav><a href="/showcase">Showcase</a> · <a href="/">Task board</a></nav>
</header>
<p>Each page runs on its own broker. Values and settings are shared by every window; holding back and slow streams act on your window only.</p>
<ul class="scenarios">
{{range .}}  <li><a href="/showcase/delivery/{{.Path}}">{{.Title}}</a>: {{.About}}</li>
{{end}}</ul>
</body>
</html>
`

var deliveryIndexTmpl = template.Must(template.New("delivery-index").Parse(deliveryIndexHTML))

// The layout's script is application code: it counts the messages each
// region receives and shows the server's state on request.
const deliveryLayoutHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/static/style.css">
<script src="/static/htmx-2.0.11.min.js"></script>
<script src="/static/htmx-ext-sse-2.2.4.min.js"></script>
{{if .TavernJS}}<script src="/static/tavern-0.0.24.min.js"></script>{{end}}
</head>
<body hx-ext="sse">
<header>
  <h1>{{.Title}}</h1>
  <nav><a href="{{.Base}}">{{.Lab}}</a> · <a href="/showcase">Showcase</a></nav>
</header>
{{template "main" .}}
<section>
  <h2>What the server saw</h2>
  <p><button type="button" id="delivery-inspect">Show server state</button></p>
  <pre id="delivery-state"></pre>
  <h3>In this window</h3>
  <ol id="delivery-events" class="events"></ol>
</section>
<script>
(function () {
  var base = "{{.Base}}", family = "{{.Family}}", conn = "{{.Conn}}";
  var log = document.getElementById("delivery-events");
  window.deliveryNote = function (text) {
    var li = document.createElement("li");
    li.textContent = new Date().toLocaleTimeString() + "  " + text;
    log.prepend(li);
    while (log.children.length > 30) log.lastChild.remove();
  };
  // Count what each counted element receives: data-count-for names the
  // element whose updates are counted.
  // htmx raises the event once per swapped element, with one detail per
  // fragment, so each fragment is counted once.
  var counts = {}, counted = new WeakSet();
  document.body.addEventListener("htmx:oobAfterSwap", function (e) {
    var id = e.detail.target && e.detail.target.id;
    if (!id || counted.has(e.detail)) return;
    counted.add(e.detail);
    counts[id] = (counts[id] || 0) + 1;
    document.querySelectorAll('[data-count-for="' + id + '"]').forEach(function (el) {
      el.textContent = counts[id];
    });
  });
  document.body.addEventListener("htmx:sseMessage", function (e) {
    if (e.detail && e.detail.type === "tavern-backpressure") window.deliveryNote("tavern-backpressure " + e.detail.data);
  });
  ["tavern:stream-degraded", "tavern:stream-restored"].forEach(function (type) {
    document.addEventListener(type, function (e) { window.deliveryNote(type + " " + JSON.stringify(e.detail)); });
  });
  document.getElementById("delivery-inspect").addEventListener("click", function () {
    fetch(base + "/" + family + "/state?conn=" + conn)
      .then(function (r) { return r.json(); })
      .then(function (s) { document.getElementById("delivery-state").textContent = JSON.stringify(s, null, 2); });
  });
})();
</script>
</body>
</html>
`
