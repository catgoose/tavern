package main

import (
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The expiry family publishes messages with a time-to-live. Live delivery is
// immediate either way; expiry removes a message from the replay cache, so a
// window opened later does not get it, and with WithAutoRemove also removes
// its element from windows that showed it.
const (
	ttNotices = "delivery/ttl/notices" // PublishWithTTL + WithAutoRemove
	ttBanner  = "delivery/ttl/banner"  // PublishOOBWithTTL
	ttStatus  = "delivery/ttl/status"  // PublishIfChangedWithTTL
	ttTeam    = "delivery/ttl/team"    // PublishToWithTTL, red team only
	ttWindow  = "delivery/ttl/window/" // one per open window, for WithTopicTTL
	ttTTL     = 3 * time.Second
	ttSweep   = 100 * time.Millisecond
	ttIdle    = time.Second // WithTopicTTL
	ttKept    = 5           // notices kept for replay
)

type ttlLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry

	mu        sync.Mutex
	seq       int
	statusSet map[string]int // "published" / "skipped"
	log       obsLog
}

func newTTLLab(d *delivery) *ttlLab {
	l := &ttlLab{
		broker:    d.broker(tavern.WithBufferSize(16), tavern.WithMessageTTLSweep(ttSweep), tavern.WithTopicTTL(ttIdle)),
		statusSet: map[string]int{},
	}
	l.broker.SetReplayPolicy(ttNotices, ttKept)
	l.broker.SetReplayPolicy(ttTeam, ttKept)
	return l
}

func (l *ttlLab) next() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return l.seq
}

func stamp() string { return time.Now().Format("15:04:05") }

// notice posts an element that removes itself from every window that shows
// it when it expires.
func (l *ttlLab) notice() {
	n := l.next()
	id := fmt.Sprintf("notice-%d", n)
	l.broker.PublishWithTTL(ttNotices,
		tavern.RenderFragments(tavern.Prepend("ttl-notices", fmt.Sprintf(`<li id=%q>Notice %d, posted %s</li>`, id, n, stamp()))),
		ttTTL, tavern.WithAutoRemove(id))
	l.log.add("notice %d posted, expires in %s", n, ttTTL)
}

// banner replaces the banner. It expires from the replay cache, but windows
// that already show it keep it.
func (l *ttlLab) banner() {
	n := l.next()
	l.broker.PublishOOBWithTTL(ttBanner, ttTTL, tavern.Replace("ttl-banner", fmt.Sprintf("Banner %d, posted %s", n, stamp())))
	l.log.add("banner %d posted, expires from replay in %s", n, ttTTL)
}

// status publishes a status only if it changed, with a TTL.
func (l *ttlLab) status(value string) bool {
	ok := l.broker.PublishIfChangedWithTTL(ttStatus,
		tavern.RenderFragments(tavern.Replace("ttl-status", `<strong>`+template.HTMLEscapeString(value)+`</strong>`)), ttTTL)
	l.mu.Lock()
	if ok {
		l.statusSet["published"]++
	} else {
		l.statusSet["skipped"]++
	}
	l.mu.Unlock()
	return ok
}

// team posts a notice for the red team only. Only red subscriptions are
// made to this topic: the replay cache is per topic, not per scope, so any
// other subscriber would be replayed the team's notices (#245).
func (l *ttlLab) team() {
	n := l.next()
	l.broker.PublishToWithTTL(ttTeam, deliveryTeam,
		tavern.RenderFragments(tavern.Prepend("ttl-team", fmt.Sprintf(`<li>Team notice %d, posted %s</li>`, n, stamp()))), ttTTL)
	l.log.add("team notice %d posted, expires from replay in %s", n, ttTTL)
}

func (l *ttlLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	sub := func(topic string, opts ...tavern.SubscribeOption) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(topic, append(opts, meta)...)
			return subOrErr(topic, ch, unsub)
		}
	}
	return openAll(sub(ttNotices), sub(ttBanner), sub(ttStatus),
		sub(ttTeam, tavern.SubWithScope(deliveryTeam)), sub(ttWindow+conn))
}

// windowTopics lists the per-window topics the broker still tracks.
func (l *ttlLab) windowTopics() []string {
	var out []string
	for t, n := range l.broker.TopicCounts() {
		if conn, ok := strings.CutPrefix(t, ttWindow); ok {
			out = append(out, fmt.Sprintf("%s (%d subscribers)", conn, n))
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) registerTTL() {
	l := a.delivery.ttl
	a.mux.HandleFunc("GET /showcase/delivery/ttl", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, ttlPage, map[string]any{"Title": "Expiring messages", "Family": "ttl", "Values": changeValues})
	})
	a.mux.HandleFunc("GET /showcase/delivery/ttl/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open})
	})
	post := func(path string, fn func()) {
		a.mux.HandleFunc("POST /showcase/delivery/ttl/"+path, func(w http.ResponseWriter, _ *http.Request) {
			fn()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	post("notice", l.notice)
	post("banner", l.banner)
	post("team", l.team)
	a.mux.HandleFunc("POST /showcase/delivery/ttl/status", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("value")
		if !slices.Contains(changeValues, v) {
			http.Error(w, "Unknown value", http.StatusBadRequest)
			return
		}
		l.status(v)
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/delivery/ttl/state", func(w http.ResponseWriter, _ *http.Request) {
		l.mu.Lock()
		status := maps.Clone(l.statusSet)
		l.mu.Unlock()
		writeJSON(w, map[string]any{"status": status, "windowTopics": l.windowTopics(), "observations": l.log.list()})
	})
}

var ttlPage = deliveryPage("ttl", `{{define "main"}}
<p class="note">Everything here expires after 3 seconds. A window open when a message is posted gets it at once. A window opened later gets it only until it expires. A notice also disappears from open windows when it expires; the banner and team notices stay on pages that show them.</p>
<section id="ttl-region" sse-connect="/showcase/delivery/ttl/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <h2>Notices</h2>
  <ul id="ttl-notices"></ul>
  <h2>Banner</h2>
  <p id="ttl-banner">No banner.</p>
  <h2>Status</h2>
  <p id="ttl-status">No status.</p>
  <h2>Team notices (red team)</h2>
  <ul id="ttl-team"></ul>
</section>
<p>
  <button hx-post="/showcase/delivery/ttl/notice" hx-swap="none">Post a notice</button>
  <button hx-post="/showcase/delivery/ttl/banner" hx-swap="none">Post a banner</button>
  <button hx-post="/showcase/delivery/ttl/team" hx-swap="none">Post a team notice</button>
  {{range .Values}}<button hx-post="/showcase/delivery/ttl/status?value={{.}}" hx-swap="none">Status {{.}}</button> {{end}}
</p>
<p class="note">Each window also subscribes to a topic of its own. After the window closes, the broker drops that topic within about a second (WithTopicTTL); Show server state lists the ones it still tracks.</p>
{{end}}`)
