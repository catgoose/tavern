package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"

	"github.com/catgoose/tavern"
)

// The change-only family publishes four fields only when they change (two
// shared, two for one team), adds activity as server-rendered components,
// and refreshes a report that is rendered only while someone has it open.
const (
	chStatus   = "delivery/render/status"    // PublishIfChangedOOB
	chNote     = "delivery/render/note"      // PublishIfChanged
	chTeam     = "delivery/render/team"      // PublishIfChangedOOBTo
	chTeamNote = "delivery/render/team-note" // PublishIfChangedTo
	chActivity = "delivery/render/activity"  // PublishOOB with components
	chCounters = "delivery/render/counters"
	chReport   = "delivery/render/report" // lazy
	chActMax   = 8                        // activity items kept on the page
)

var changeValues = []string{"green", "amber", "red"}

type changeField struct {
	topic, element, label string
	scoped, oob           bool
}

var changeFields = map[string]changeField{
	"status":    {chStatus, "render-status", "Status", false, true},
	"note":      {chNote, "render-note", "Note", false, false},
	"team":      {chTeam, "render-team", "Team status", true, true},
	"team-note": {chTeamNote, "render-team-note", "Team note", true, false},
}

type changeLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry // main streams
	panels connRegistry // report panel streams

	pubMu sync.Mutex // orders publications against stream snapshots
	mu    sync.Mutex
	value map[string]string // field → current value
	sent  map[string]int    // field → published
	same  map[string]int    // field → skipped as unchanged
	acts  []int             // activity sequence numbers on the page, newest first
	seq   int
	lazy  struct{ calls, renders, published, rejected int }
	log   obsLog
}

func newChangeLab(d *delivery) *changeLab {
	l := &changeLab{
		broker: d.broker(tavern.WithBufferSize(16)),
		value:  map[string]string{},
		sent:   map[string]int{},
		same:   map[string]int{},
	}
	for f := range changeFields {
		l.value[f] = "green"
	}
	return l
}

func (l *changeLab) fieldHTML(f string) string {
	return fmt.Sprintf(`<strong data-value="%[1]s">%[1]s</strong>`, template.HTMLEscapeString(l.value[f]))
}

// set changes a field and publishes it if its content changed.
func (l *changeLab) set(field, value string) bool {
	f := changeFields[field]
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	l.value[field] = value
	frag := tavern.Replace(f.element, l.fieldHTML(field))
	l.mu.Unlock()
	var changed bool
	switch {
	case f.oob && f.scoped:
		changed = l.broker.PublishIfChangedOOBTo(f.topic, deliveryTeam, frag)
	case f.oob:
		changed = l.broker.PublishIfChangedOOB(f.topic, frag)
	case f.scoped:
		changed = l.broker.PublishIfChangedTo(f.topic, deliveryTeam, tavern.RenderFragments(frag))
	default:
		changed = l.broker.PublishIfChanged(f.topic, tavern.RenderFragments(frag))
	}
	l.mu.Lock()
	if changed {
		l.sent[field]++
	} else {
		l.same[field]++
	}
	l.mu.Unlock()
	l.publishCountersLocked()
	return changed
}

// clearDedup forgets a field's last published content, so the next set
// publishes even if nothing changed.
func (l *changeLab) clearDedup(field string) {
	l.broker.ClearDedup(changeFields[field].topic)
	l.log.add("dedup cleared for %s", field)
}

// activityItem is one server-rendered activity entry.
type activityItem struct {
	seq    int
	text   string
	broken bool
}

var errActivityBroken = errors.New("this item cannot be rendered")

func (a activityItem) Render(_ context.Context, w io.Writer) error {
	if a.broken {
		return errActivityBroken
	}
	_, err := fmt.Fprintf(w, `<li id="act-%d">#%d %s</li>`, a.seq, a.seq, template.HTMLEscapeString(a.text))
	return err
}

// addActivity prepends an item and removes the oldest beyond chActMax. A
// broken item is rendered first with RenderComponentErr, and its error is
// reported instead of publishing it.
func (l *changeLab) addActivity(broken bool) error {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	l.seq++
	item := activityItem{seq: l.seq, text: "status " + l.value["status"], broken: broken}
	l.mu.Unlock()
	if broken {
		if _, err := tavern.RenderComponentErr(item); err != nil {
			l.mu.Lock()
			l.lazy.rejected++
			l.mu.Unlock()
			l.log.add("activity #%d not published: %v", item.seq, err)
			l.publishCountersLocked()
			return err
		}
	}
	l.mu.Lock()
	frags := []tavern.Fragment{tavern.PrependComponent("render-activity", item)}
	l.acts = append([]int{item.seq}, l.acts...)
	if len(l.acts) > chActMax {
		frags = append(frags, tavern.Delete(fmt.Sprintf("act-%d", l.acts[chActMax])))
		l.acts = l.acts[:chActMax]
	}
	l.mu.Unlock()
	l.broker.PublishOOB(chActivity, frags...)
	l.publishCountersLocked()
	return nil
}

func (l *changeLab) reportFragments() []tavern.Fragment {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lazy.renders++
	return []tavern.Fragment{tavern.Replace("render-report", fmt.Sprintf(
		`<p>Status <strong>%s</strong>, note <strong>%s</strong>, %d activity items.</p>`,
		template.HTMLEscapeString(l.value["status"]), template.HTMLEscapeString(l.value["note"]), len(l.acts)))}
}

// refreshReport asks for the report to be published. The render function
// runs only if the report topic has subscribers.
func (l *changeLab) refreshReport(onlyChanged, team bool) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.mu.Lock()
	l.lazy.calls++
	l.mu.Unlock()
	var published bool
	switch {
	case onlyChanged && team:
		published = l.broker.PublishLazyIfChangedOOBTo(chReport, deliveryTeam, l.reportFragments)
	case onlyChanged:
		published = l.broker.PublishLazyIfChangedOOB(chReport, l.reportFragments)
	default:
		// The plain variants report nothing; they render and publish when
		// the topic has subscribers.
		published = l.broker.HasSubscribers(chReport)
		if team {
			l.broker.PublishLazyOOBTo(chReport, deliveryTeam, l.reportFragments)
		} else {
			l.broker.PublishLazyOOB(chReport, l.reportFragments)
		}
	}
	l.mu.Lock()
	if published {
		l.lazy.published++
	}
	l.mu.Unlock()
	l.publishCountersLocked()
}

// publishCountersLocked publishes the counters; the caller holds pubMu.
func (l *changeLab) publishCountersLocked() {
	l.broker.PublishOOB(chCounters, tavern.Replace("render-counters", l.countersHTML()))
}

func (l *changeLab) countersHTML() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf(`<span data-sent="%d" data-same="%d">published %d, skipped as unchanged %d</span>; report: <span data-calls="%d" data-renders="%d">%d refreshes asked, %d rendered, %d published</span>; broken items refused: <span data-rejected="%d">%d</span>`,
		sum(l.sent), sum(l.same), sum(l.sent), sum(l.same),
		l.lazy.calls, l.lazy.renders, l.lazy.calls, l.lazy.renders, l.lazy.published, l.lazy.rejected, l.lazy.rejected)
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// snapshot is every region's current content, written before any change.
func (l *changeLab) snapshot() string {
	l.mu.Lock()
	var frags []tavern.Fragment
	for f, def := range changeFields {
		frags = append(frags, tavern.Replace(def.element, l.fieldHTML(f)))
	}
	var items string
	for _, seq := range l.acts {
		items += fmt.Sprintf(`<li id="act-%d">#%d</li>`, seq, seq)
	}
	frags = append(frags, tavern.Replace("render-activity", items))
	l.mu.Unlock()
	frags = append(frags, tavern.Replace("render-counters", l.countersHTML()))
	return tavern.NewSSEMessage("oob", tavern.RenderFragments(frags...)).String()
}

func (l *changeLab) openMain(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	snap := l.snapshot()
	sub := func(topic string, opts ...tavern.SubscribeOption) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(topic, append(opts, meta)...)
			return subOrErr(topic, ch, unsub)
		}
	}
	team := tavern.SubWithScope(deliveryTeam)
	return openAll(
		sub(chStatus, tavern.SubWithSnapshot(func() string { return snap })),
		sub(chNote), sub(chActivity), sub(chCounters),
		sub(chTeam, team), sub(chTeamNote, team),
	)
}

func (l *changeLab) openReport(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	return openAll(
		func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(chReport, meta)
			return subOrErr("report", ch, unsub)
		},
		func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(chReport, meta, tavern.SubWithScope(deliveryTeam))
			return subOrErr("team-report", ch, unsub)
		},
	)
}

func (a *App) registerChange() {
	l := a.delivery.change
	a.mux.HandleFunc("GET /showcase/delivery/render", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, changePage, map[string]any{"Title": "Change-only and lazy rendering", "Family": "render", "Values": changeValues})
	})
	a.mux.HandleFunc("GET /showcase/delivery/render/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.openMain})
	})
	a.mux.HandleFunc("GET /showcase/delivery/render/report-events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.panels, open: l.openReport})
	})
	a.mux.HandleFunc("GET /showcase/delivery/render/report-panel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Get("open") != "1" {
			return
		}
		if err := changePanelTmpl.Execute(w, newConnID()); err != nil {
			a.logger.Error("render report panel", "err", err)
		}
	})
	a.mux.HandleFunc("POST /showcase/delivery/render/set", func(w http.ResponseWriter, r *http.Request) {
		field, value := r.URL.Query().Get("field"), r.URL.Query().Get("value")
		if _, ok := changeFields[field]; !ok || !slices.Contains(changeValues, value) {
			http.Error(w, "Unknown field or value", http.StatusBadRequest)
			return
		}
		l.set(field, value)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/delivery/render/clear-dedup", func(w http.ResponseWriter, r *http.Request) {
		field := r.URL.Query().Get("field")
		if _, ok := changeFields[field]; !ok {
			http.Error(w, "Unknown field", http.StatusBadRequest)
			return
		}
		l.clearDedup(field)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/delivery/render/activity", func(w http.ResponseWriter, r *http.Request) {
		_ = l.addActivity(r.URL.Query().Get("broken") == "1") // a refusal is logged and counted
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /showcase/delivery/render/report", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		l.refreshReport(q.Get("changed") == "1", q.Get("team") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/delivery/render/state", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		v := map[string]any{
			"values": maps.Clone(l.value), "published": maps.Clone(l.sent), "skipped": maps.Clone(l.same),
			"report":  map[string]int{"asked": l.lazy.calls, "rendered": l.lazy.renders, "published": l.lazy.published},
			"refused": l.lazy.rejected, "activity": len(l.acts),
		}
		l.mu.Unlock()
		v["reportSubscribers"] = l.broker.HasSubscribers(chReport)
		v["observations"] = l.log.list()
		writeJSON(w, v)
	})
}

var changePage = deliveryPage("render", `{{define "main"}}
<p class="note">Each field is published only when its content changes; setting the same value again is skipped. Team fields go only to the red team, which this page joins. The report is rendered only while some window has its panel open.</p>
<section id="render-region" sse-connect="/showcase/delivery/render/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <table>
    <tr><th>Status</th><td><span id="render-status">…</span></td><td data-count-for="render-status">0</td></tr>
    <tr><th>Note</th><td><span id="render-note">…</span></td><td data-count-for="render-note">0</td></tr>
    <tr><th>Team status</th><td><span id="render-team">…</span></td><td data-count-for="render-team">0</td></tr>
    <tr><th>Team note</th><td><span id="render-team-note">…</span></td><td data-count-for="render-team-note">0</td></tr>
  </table>
  <p id="render-counters"></p>
  <h2>Activity</h2>
  <ol id="render-activity"></ol>
</section>
<p>{{range .Values}}<button hx-post="/showcase/delivery/render/set?field=status&amp;value={{.}}" hx-swap="none">Status {{.}}</button> {{end}}
  <button hx-post="/showcase/delivery/render/clear-dedup?field=status" hx-swap="none">Forget the last status</button>
</p>
<p>{{range .Values}}<button hx-post="/showcase/delivery/render/set?field=note&amp;value={{.}}" hx-swap="none">Note {{.}}</button> {{end}}</p>
<p>{{range .Values}}<button hx-post="/showcase/delivery/render/set?field=team&amp;value={{.}}" hx-swap="none">Team status {{.}}</button> {{end}}</p>
<p>{{range .Values}}<button hx-post="/showcase/delivery/render/set?field=team-note&amp;value={{.}}" hx-swap="none">Team note {{.}}</button> {{end}}</p>
<p>
  <button hx-post="/showcase/delivery/render/activity" hx-swap="none">Add activity</button>
  <button hx-post="/showcase/delivery/render/activity?broken=1" hx-swap="none">Add an item that fails to render</button>
</p>
<h2>Report</h2>
<p>
  <button hx-get="/showcase/delivery/render/report-panel?open=1" hx-target="#render-panel" hx-swap="innerHTML">Open the report panel</button>
  <button hx-get="/showcase/delivery/render/report-panel" hx-target="#render-panel" hx-swap="innerHTML">Close it</button>
  <button hx-post="/showcase/delivery/render/report" hx-swap="none">Refresh the report</button>
  <button hx-post="/showcase/delivery/render/report?changed=1" hx-swap="none">Refresh it if changed</button>
  <button hx-post="/showcase/delivery/render/report?team=1" hx-swap="none">Refresh the team report</button>
  <button hx-post="/showcase/delivery/render/report?team=1&amp;changed=1" hx-swap="none">Refresh the team report if changed</button>
</p>
<div id="render-panel"></div>
{{end}}`)

var changePanelTmpl = template.Must(template.New("report-panel").Parse(
	`<section id="render-report-panel" sse-connect="/showcase/delivery/render/report-events?conn={{.}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <div id="render-report"><p>Waiting for a refresh.</p></div>
  <p>Report updates received: <span data-count-for="render-report">0</span></p>
</section>`))
