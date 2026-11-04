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
	flList     = "orders:list"
	flSummary  = "orders:summary"
	flAudit    = "audit:log"
	flKept     = 6 // orders kept, and shown unless held back
	flAuditMax = 8 // audit entries kept and shown
	flTick     = 2 * time.Second
	unchecked  = `<span class="mw">unchecked</span>`
	checked    = `<span class="mw">checked by orders:*</span>`
)

var flowItems = []string{"tea", "coffee", "cake"}

type order struct {
	ID   int
	Item string
	Test bool
}

type auditEntry struct {
	id   int
	html string
}

type flowLab struct {
	d      *delivery
	broker *tavern.SSEBroker
	reg    connRegistry

	// pubMu orders each mutation with its publishes, each derived summary,
	// and each window's snapshot and subscription, so they reach every
	// subscriber in model order. Lock order is pubMu, then mu. The mutation
	// handler and middleware run inside pubMu and take only mu.
	pubMu    sync.Mutex
	mu       sync.Mutex
	orders   []order      // newest first
	audit    []auditEntry // newest first
	seq      int
	auditSeq int
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
			out, ok := checkOrders(msg)
			if !ok {
				l.count("filtered by orders:* middleware")
				return
			}
			next(topic, out)
		}
	})
	b.OnMutate("orders", l.onOrder)
	b.After(flList, l.publishSummary)
	b.OnFirstSubscriber(flSummary, func(string) { l.startWorker() })
	b.OnLastUnsubscribe(flSummary, func(string) { l.stopWorker() })
	return l
}

// checkOrders is what the orders:* middleware does to a message: it holds
// back test orders and marks the rest as checked. Window snapshots, which
// are not published, pass their order parts through it too.
func checkOrders(msg string) (string, bool) {
	if strings.Contains(msg, `data-test="true"`) {
		return "", false
	}
	return strings.ReplaceAll(msg, unchecked, checked), true
}

func (l *flowLab) count(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[step]++
}

type mutation struct {
	order        order
	removed      []int // orders beyond flKept
	audit        auditEntry
	removedAudit []int // audit entries beyond flAuditMax
}

// add changes the model and then signals the mutation, all under pubMu so
// a later mutation cannot publish first. The model lock is released before
// NotifyMutate: the handler runs in this goroutine and takes it itself.
func (l *flowLab) add(item string, test bool) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	l.seq++
	o := order{ID: l.seq, Item: item, Test: test}
	m := mutation{order: o}
	l.orders = append([]order{o}, l.orders...)
	for len(l.orders) > flKept {
		// A test order was never shown, so there is no row to remove.
		if old := l.orders[len(l.orders)-1]; !old.Test {
			m.removed = append(m.removed, old.ID)
		}
		l.orders = l.orders[:len(l.orders)-1]
	}
	note := ""
	if o.Test {
		note = " (test)"
	}
	l.auditSeq++
	m.audit = auditEntry{l.auditSeq, fmt.Sprintf(`<li id="audit-%d">order #%d added%s %s</li>`, l.auditSeq, o.ID, note, unchecked)}
	l.audit = append([]auditEntry{m.audit}, l.audit...)
	for len(l.audit) > flAuditMax {
		m.removedAudit = append(m.removedAudit, l.audit[len(l.audit)-1].id)
		l.audit = l.audit[:len(l.audit)-1]
	}
	l.counts["mutations"]++
	l.mu.Unlock()
	l.broker.NotifyMutate("orders", tavern.MutationEvent{ID: strconv.Itoa(o.ID), Data: m})
}

// visibleRows is the rows a window shows, newest first.
func visibleRows(orders []order) []string {
	out := []string{}
	for _, o := range orders {
		if row, ok := checkOrders(rowHTML(o)); ok {
			out = append(out, row)
		}
	}
	return out
}

// listContents replaces a list's items, keeping the list element itself.
func listContents(id, items string) tavern.Fragment {
	return tavern.Fragment{ID: id, Swap: "innerHTML", HTML: items}
}

func rowHTML(o order) string {
	test := ""
	if o.Test {
		test = ` data-test="true"`
	}
	return fmt.Sprintf(`<li id="order-%d"%s>#%d %s %s</li>`, o.ID, test, o.ID, template.HTMLEscapeString(o.Item), unchecked)
}

func deletes(prefix string, ids []int) []tavern.Fragment {
	var out []tavern.Fragment
	for _, id := range ids {
		out = append(out, tavern.Delete(fmt.Sprintf("%s-%d", prefix, id)))
	}
	return out
}

// onOrder publishes the new row, the removal of rows and audit entries
// beyond their limits, and the audit entry.
func (l *flowLab) onOrder(ev tavern.MutationEvent) {
	m := ev.Data.(mutation)
	l.count("mutation handler")
	l.broker.PublishOOB(flList, tavern.Prepend("order-rows", rowHTML(m.order)))
	// Removals go separately, so holding back a test row cannot hold them.
	if len(m.removed) > 0 {
		l.broker.PublishOOB(flList, deletes("order", m.removed)...)
	}
	l.broker.PublishOOB(flAudit, append([]tavern.Fragment{tavern.Prepend("audit-rows", m.audit.html)}, deletes("audit", m.removedAudit)...)...)
}

func (l *flowLab) summaryHTML() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	byItem := map[string]int{}
	for _, o := range l.orders {
		if !o.Test {
			n++
			byItem[o.Item]++
		}
	}
	var parts []string
	for _, item := range flowItems {
		parts = append(parts, fmt.Sprintf("%s %d", item, byItem[item]))
	}
	return fmt.Sprintf(`<span data-orders="%d">%d orders shown: %s</span> %s`, n, n, strings.Join(parts, ", "), unchecked)
}

// publishSummary is the After hook on the list: a derived update, published
// as its own operation after each list publish that went out. It runs in
// its own goroutine and takes pubMu, so it waits for the mutation that
// triggered it and publishes the model as it is then; an older summary
// cannot follow a newer one.
func (l *flowLab) publishSummary() {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.count("derived summaries")
	l.broker.PublishOOB(flSummary, tavern.Replace("order-summary", l.summaryHTML()))
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
	if !l.d.goBounded(func(context.Context) {
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
	}) {
		l.worker = nil // the lab is stopping
		cancel()
	}
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

// snapshotLocked is the window's starting state, as the middleware would
// have shown it; the caller holds pubMu.
func (l *flowLab) snapshotLocked() string {
	l.mu.Lock()
	var rows, audit strings.Builder
	for _, row := range visibleRows(l.orders) {
		rows.WriteString(row)
	}
	for _, e := range l.audit {
		audit.WriteString(e.html)
	}
	l.mu.Unlock()
	summary, _ := checkOrders(l.summaryHTML())
	return tavern.NewSSEMessage("oob", tavern.RenderFragments(
		listContents("order-rows", rows.String()),
		tavern.Replace("order-summary", summary),
		listContents("audit-rows", audit.String()),
	)).String()
}

// open takes the snapshot and subscribes under pubMu, so every later
// publish is queued after it; the snapshot is written before any of them.
func (l *flowLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), snapshot string, err error) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	snapshot = l.snapshotLocked()
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	sub := func(topic string) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(topic, meta)
			return subOrErr(topic, ch, unsub)
		}
	}
	msgs, unsubscribe, err = openAll(sub(flList), sub(flSummary), sub(flAudit))
	return msgs, unsubscribe, snapshot, err
}

func (a *App) registerFlow() {
	l := a.delivery.flow
	a.mux.HandleFunc("GET /showcase/delivery/flow", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, flowPage, map[string]any{"Title": "Reactive workflow", "Family": "flow", "Items": flowItems})
	})
	a.mux.HandleFunc("GET /showcase/delivery/flow/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, openSnapshot: l.open})
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
			"workerRunning": l.worker != nil, "observations": l.log.list(), "visible": visibleRows(l.orders),
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
