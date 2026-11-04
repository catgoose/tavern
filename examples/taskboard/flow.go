package main

import (
	"context"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The workflow family connects a model change to the page in separate steps:
// the handler adds a row through NotifyMutate and OnMutate, an After hook on
// the list topic then publishes a derived summary, and middleware checks
// order topics on the way out. Topic names use colons, which UseTopics
// patterns match segment by segment.
const (
	flList    = "orders:list"
	flSummary = "orders:summary"
	flAudit   = "audit:log"
	flKept    = 6 // orders on the page
	flTick    = 2 * time.Second
	unchecked = `<span class="mw">unchecked</span>`
	checked   = `<span class="mw">checked by orders:*</span>`
)

var flowItems = []string{"tea", "coffee", "cake"}

type order struct {
	ID   int
	Item string
	Test bool
}

type flowLab struct {
	d      *delivery
	broker *tavern.SSEBroker
	reg    connRegistry

	mu       sync.Mutex
	orders   []order // newest first
	seq      int
	counts   map[string]int // what happened, by step
	seen     map[string]int // topic → publishes seen by the global middleware
	worker   context.CancelFunc
	workerID int
	log      obsLog
}

func newFlowLab(d *delivery) *flowLab {
	l := &flowLab{d: d, broker: d.broker(tavern.WithBufferSize(16)), counts: map[string]int{}, seen: map[string]int{}}
	b := l.broker
	b.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			l.mu.Lock()
			l.seen[topic]++
			l.mu.Unlock()
			next(topic, msg)
		}
	})
	b.UseTopics("orders:*", func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			if strings.Contains(msg, `data-test="true"`) {
				l.count("filtered by orders:* middleware")
				return
			}
			next(topic, strings.ReplaceAll(msg, unchecked, checked))
		}
	})
	b.OnMutate("orders", l.onOrder)
	b.After(flList, l.publishSummary)
	b.OnFirstSubscriber(flSummary, func(string) { l.startWorker() })
	b.OnLastUnsubscribe(flSummary, func(string) { l.stopWorker() })
	return l
}

func (l *flowLab) count(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[step]++
}

// add changes the model and then signals the mutation. The model lock is
// released first: the handlers run synchronously in this goroutine and
// read the model themselves.
func (l *flowLab) add(item string, test bool) {
	l.mu.Lock()
	l.seq++
	o := order{ID: l.seq, Item: item, Test: test}
	l.orders = append([]order{o}, l.orders...)
	var removed []int
	for len(l.orders) > flKept {
		removed = append(removed, l.orders[len(l.orders)-1].ID)
		l.orders = l.orders[:len(l.orders)-1]
	}
	l.counts["mutations"]++
	l.mu.Unlock()
	l.broker.NotifyMutate("orders", tavern.MutationEvent{ID: strconv.Itoa(o.ID), Data: mutation{order: o, removed: removed}})
}

type mutation struct {
	order   order
	removed []int
}

// onOrder publishes the new row (and removes rows beyond flKept) and an
// audit entry.
func (l *flowLab) onOrder(ev tavern.MutationEvent) {
	m := ev.Data.(mutation)
	o := m.order
	l.count("mutation handler")
	test := ""
	if o.Test {
		test = ` data-test="true"`
	}
	l.broker.PublishOOB(flList, tavern.Prepend("order-rows", fmt.Sprintf(
		`<li id="order-%d"%s>#%d %s %s</li>`, o.ID, test, o.ID, template.HTMLEscapeString(o.Item), unchecked)))
	// Removals go separately, so holding back a test row cannot hold them.
	if len(m.removed) > 0 {
		var dels []tavern.Fragment
		for _, id := range m.removed {
			dels = append(dels, tavern.Delete(fmt.Sprintf("order-%d", id)))
		}
		l.broker.PublishOOB(flList, dels...)
	}
	note := ""
	if o.Test {
		note = " (test)"
	}
	l.broker.PublishOOB(flAudit, tavern.Prepend("audit-rows", fmt.Sprintf(`<li>order #%d added%s %s</li>`, o.ID, note, unchecked)))
}

// publishSummary is the After hook on the list: a derived update, published
// as its own operation after each list publish that went out.
func (l *flowLab) publishSummary() {
	l.mu.Lock()
	n := 0
	byItem := map[string]int{}
	for _, o := range l.orders {
		if !o.Test {
			n++
			byItem[o.Item]++
		}
	}
	l.counts["derived summaries"]++
	l.mu.Unlock()
	var parts []string
	for _, item := range flowItems {
		parts = append(parts, fmt.Sprintf("%s %d", item, byItem[item]))
	}
	l.broker.PublishOOB(flSummary, tavern.Replace("order-summary",
		fmt.Sprintf(`<span data-orders="%d">%d orders shown: %s</span> %s`, n, n, strings.Join(parts, ", "), unchecked)))
}

// startWorker runs while the summary has subscribers: it refreshes the
// summary's clock every flTick. It is started by the first subscriber and
// stopped by the last unsubscribe; both hooks run in their own goroutines.
func (l *flowLab) startWorker() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.worker != nil {
		return
	}
	ctx, cancel := context.WithCancel(l.d.ctx)
	l.worker = cancel
	l.workerID++
	id := l.workerID
	l.counts["worker starts"]++
	l.log.add("first subscriber to %s: worker %d started", flSummary, id)
	l.d.goBounded(func(context.Context) {
		defer func() {
			l.mu.Lock()
			if l.workerID == id && l.worker != nil {
				l.worker()
				l.worker = nil
			}
			l.mu.Unlock()
		}()
		for sleepCtx(ctx, flTick) {
			if !l.broker.HasSubscribers(flSummary) {
				return
			}
			l.broker.PublishOOB(flSummary, tavern.Replace("order-clock", "worker "+strconv.Itoa(id)+" at "+time.Now().Format("15:04:05")))
		}
	})
}

func (l *flowLab) stopWorker() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.worker == nil || l.broker.HasSubscribers(flSummary) {
		return
	}
	l.worker()
	l.worker = nil
	l.counts["worker stops"]++
	l.log.add("last unsubscribe from %s: worker %d stopped", flSummary, l.workerID)
}

func (l *flowLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	sub := func(topic string) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(topic, meta)
			return subOrErr(topic, ch, unsub)
		}
	}
	return openAll(sub(flList), sub(flSummary), sub(flAudit))
}

func (a *App) registerFlow() {
	l := a.delivery.flow
	a.mux.HandleFunc("GET /showcase/delivery/flow", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, flowPage, map[string]any{"Title": "Reactive workflow", "Family": "flow", "Items": flowItems})
	})
	a.mux.HandleFunc("GET /showcase/delivery/flow/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open})
	})
	a.mux.HandleFunc("POST /showcase/delivery/flow/order", func(w http.ResponseWriter, r *http.Request) {
		item := r.URL.Query().Get("item")
		if !slices.Contains(flowItems, item) {
			http.Error(w, "Unknown item", http.StatusBadRequest)
			return
		}
		l.add(item, r.URL.Query().Get("test") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/delivery/flow/state", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		writeJSON(w, map[string]any{
			"steps": maps.Clone(l.counts), "seenByGlobalMiddleware": maps.Clone(l.seen),
			"workerRunning": l.worker != nil, "observations": l.log.list(),
		})
	})
}

var flowPage = deliveryPage("flow", `{{define "main"}}
<p class="note">Adding an order changes the model and calls NotifyMutate. The order handler publishes the row and an audit entry. An After hook on the list topic then publishes the summary as a separate step. Middleware on <code>orders:*</code> marks rows and summaries as checked and holds back test orders. The audit log is not an order topic, so its entries stay unchecked. A worker refreshes the summary's clock while some window shows the summary.</p>
<section id="flow-region" sse-connect="/showcase/delivery/flow/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <h2>Orders</h2>
  <ol id="order-rows"></ol>
  <h2>Summary</h2>
  <p id="order-summary">No summary yet.</p>
  <p id="order-clock">Worker not seen yet.</p>
  <h2>Audit log</h2>
  <ul id="audit-rows"></ul>
</section>
<p>
  {{range .Items}}<button hx-post="/showcase/delivery/flow/order?item={{.}}" hx-swap="none">Order {{.}}</button> {{end}}
  <button hx-post="/showcase/delivery/flow/order?item=tea&amp;test=1" hx-swap="none">Place a test order</button>
</p>
{{end}}`)
