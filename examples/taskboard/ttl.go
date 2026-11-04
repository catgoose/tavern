package main

import (
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"regexp"
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
	ttTeam    = "delivery/ttl/team"    // PublishToWithTTL + WithAutoRemove, for the red team
	ttWindow  = "delivery/ttl/window/" // one per open window, for WithTopicTTL
	ttTTL     = 3 * time.Second
	ttSweep   = 100 * time.Millisecond
	ttIdle    = time.Second // WithTopicTTL
	ttKept    = 5           // notices showing at once, all kept for replay
)

type ttlLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry

	mu        sync.Mutex
	seq       int
	showing   map[string]map[string]bool // topic → element IDs posted and not yet removed by expiry
	statusSet map[string]int  // "published" / "skipped"
	log       obsLog
}

func newTTLLab(d *delivery) *ttlLab {
	l := &ttlLab{
		broker:    d.broker(tavern.WithBufferSize(16), tavern.WithMessageTTLSweep(ttSweep), tavern.WithTopicTTL(ttIdle)),
		showing:   map[string]map[string]bool{ttNotices: {}, ttTeam: {}},
		statusSet: map[string]int{},
	}
	l.broker.SetReplayPolicy(ttNotices, ttKept)
	l.broker.SetReplayPolicy(ttTeam, ttKept)
	// The sweeper publishes a notice's removal when it expires (to the team
	// alone for a team notice); seeing that publish is how the app knows the
	// notice has gone.
	l.broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			if ids := l.showingFor(topic); ids != nil {
				if m := removedNotice.FindStringSubmatch(msg); m != nil {
					l.mu.Lock()
					delete(ids, m[1])
					l.mu.Unlock()
				}
			}
			next(topic, msg)
		}
	})
	return l
}

func (l *ttlLab) next() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return l.seq
}

func stamp() string { return time.Now().Format("15:04:05") }

var removedNotice = regexp.MustCompile(`^<div id="((?:notice|team)-\d+)" hx-swap-oob="delete"></div>$`)

func (l *ttlLab) showingFor(topic string) map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.showing[topic]
}

// notice posts an element that removes itself from every window that shows
// it when it expires. The replay cache keeps ttKept entries, and an entry
// pushed out of it by a newer one is never swept, so its element would
// never be removed. A notice is therefore refused while ttKept are showing.
func (l *ttlLab) notice() (posted bool) {
	l.mu.Lock()
	if len(l.showing[ttNotices]) >= ttKept {
		l.mu.Unlock()
		l.log.add("notice refused: %d are showing", ttKept)
		return false
	}
	l.seq++
	n := l.seq
	id := fmt.Sprintf("notice-%d", n)
	l.showing[ttNotices][id] = true
	// Publishing under mu keeps the count and the cache in step; the
	// middleware above takes mu only for removals, which this publish is not.
	l.broker.PublishWithTTL(ttNotices,
		tavern.RenderFragments(tavern.Prepend("ttl-notices", fmt.Sprintf(`<li id=%q>Notice %d, posted %s</li>`, id, n, stamp()))),
		ttTTL, tavern.WithAutoRemove(id))
	l.mu.Unlock()
	l.log.add("notice %d posted, expires in %s", n, ttTTL)
	return true
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

// team posts a notice for the red team that removes itself when it
// expires. Every window subscribes to the same topic; only the red team's
// windows receive it, are replayed it while it lasts, and receive its
// removal. Like notices, at most ttKept show at once.
func (l *ttlLab) team() (posted bool) {
	l.mu.Lock()
	if len(l.showing[ttTeam]) >= ttKept {
		l.mu.Unlock()
		l.log.add("team notice refused: %d are showing", ttKept)
		return false
	}
	l.seq++
	n := l.seq
	id := fmt.Sprintf("team-%d", n)
	l.showing[ttTeam][id] = true
	l.broker.PublishToWithTTL(ttTeam, deliveryTeam,
		tavern.RenderFragments(tavern.Prepend("ttl-team", fmt.Sprintf(`<li id=%q>Team notice %d, posted %s</li>`, id, n, stamp()))),
		ttTTL, tavern.WithAutoRemove(id))
	l.mu.Unlock()
	l.log.add("team notice %d posted for the %s team, expires in %s", n, deliveryTeam, ttTTL)
	return true
}

// ttlAudiences are the ways a window can join the team topic.
var ttlAudiences = map[string]string{"red": deliveryTeam, "blue": "blue", "none": ""}

func (l *ttlLab) open(conn, team string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	sub := func(topic string, opts ...tavern.SubscribeOption) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := l.broker.SubscribeWith(topic, append(opts, meta)...)
			return subOrErr(topic, ch, unsub)
		}
	}
	teamSub := func(team string) func() (keyedSub, error) {
		if team == "" {
			return sub(ttTeam)
		}
		return sub(ttTeam, tavern.SubWithScope(team))
	}
	return openAll(sub(ttNotices), sub(ttBanner), sub(ttStatus),
		teamSub(team), sub(ttWindow+conn))
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
	audience := func(r *http.Request) (name, scope string, ok bool) {
		name = r.URL.Query().Get("team")
		if name == "" {
			name = "red"
		}
		scope, ok = ttlAudiences[name]
		return name, scope, ok
	}
	a.mux.HandleFunc("GET /showcase/delivery/ttl", func(w http.ResponseWriter, r *http.Request) {
		team, _, ok := audience(r)
		if !ok {
			http.Error(w, "Unknown team", http.StatusBadRequest)
			return
		}
		a.renderDeliveryPage(w, ttlPage, map[string]any{"Title": "Expiring messages", "Family": "ttl", "Values": changeValues, "Team": team})
	})
	a.mux.HandleFunc("GET /showcase/delivery/ttl/events", func(w http.ResponseWriter, r *http.Request) {
		_, scope, ok := audience(r)
		if !ok {
			http.Error(w, "Unknown team", http.StatusBadRequest)
			return
		}
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: func(conn string) (<-chan tavern.TopicMessage, func(), error) { return l.open(conn, scope) }})
	})
	post := func(path string, fn func()) {
		a.mux.HandleFunc("POST /showcase/delivery/ttl/"+path, func(w http.ResponseWriter, _ *http.Request) {
			fn()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	a.mux.HandleFunc("POST /showcase/delivery/ttl/notice", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !l.notice() {
			fmt.Fprintf(w, "%d notices are showing; wait for one to expire.", ttKept)
			return
		}
		fmt.Fprint(w, "Posted.")
	})
	post("banner", l.banner)
	a.mux.HandleFunc("POST /showcase/delivery/ttl/team", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !l.team() {
			fmt.Fprintf(w, "%d team notices are showing; wait for one to expire.", ttKept)
			return
		}
		fmt.Fprint(w, "Posted for the red team.")
	})
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
<p class="note">Everything here expires after 3 seconds. A window open when a message is posted gets it at once. A window opened later gets it only until it expires. A notice also disappears from open windows when it expires; the banner stays on pages that show it. At most five notices show at once: the replay cache keeps five, and a notice pushed out of it would never be removed, so a sixth waits until one expires.</p>
<p class="note">Team notices are for the red team. Every window subscribes to the team topic: this one {{if eq .Team "none"}}without a team{{else}}as the {{.Team}} team{{end}}. Only red windows receive a team notice, are replayed it while it lasts, and see it removed when it expires. <a href="/showcase/delivery/ttl?team=red">Open as red</a> · <a href="/showcase/delivery/ttl?team=blue">as blue</a> · <a href="/showcase/delivery/ttl?team=none">without a team</a></p>
<section id="ttl-region" sse-connect="/showcase/delivery/ttl/events?team={{.Team}}&amp;conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <h2>Notices</h2>
  <ul id="ttl-notices"></ul>
  <h2>Banner</h2>
  <p id="ttl-banner">No banner.</p>
  <h2>Status</h2>
  <p id="ttl-status">No status.</p>
  <h2>Team notices (red team)</h2>
  <ul id="ttl-team" data-team="{{.Team}}"></ul>
</section>
<p>
  <button hx-post="/showcase/delivery/ttl/notice" hx-target="#ttl-notice-result">Post a notice</button> <span id="ttl-notice-result"></span>
  <button hx-post="/showcase/delivery/ttl/banner" hx-swap="none">Post a banner</button>
  <button hx-post="/showcase/delivery/ttl/team" hx-target="#ttl-team-result">Post a team notice</button> <span id="ttl-team-result"></span>
  {{range .Values}}<button hx-post="/showcase/delivery/ttl/status?value={{.}}" hx-swap="none">Status {{.}}</button> {{end}}
</p>
<p class="note">Each window also subscribes to a topic of its own. After the window closes, the broker drops that topic within about a second (WithTopicTTL); Show server state lists the ones it still tracks.</p>
{{end}}`)
