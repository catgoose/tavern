package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/presence"
)

// The presence page tracks who is in one room. The tracker keys presence by
// user, so the app counts each user's open tabs: the first tab joins, the
// last one to close leaves, and a user stays while any tab sends
// heartbeats. Every tab here is one stream in this process; this is not
// presence across servers.
const (
	prRoom      = "ops/room"
	prSuffix    = "/people"
	prTarget    = "people"
	prStale     = 2 * time.Second // the tracker sweeps every second
	prHeartbeat = 500 * time.Millisecond
)

var prStatuses = []string{"here", "busy", "away"}

type presenceTab struct {
	user   string
	paused atomic.Bool // heartbeats stopped
}

type presenceLab struct {
	o       *ops
	broker  *tavern.SSEBroker
	tracker *presence.Tracker
	reg     connRegistry

	// renderMu orders presence publishes and window snapshots. Every
	// publish to the presence topic is re-rendered under it from the
	// tracker's current list (see the middleware), so a render taken
	// earlier can never be sent after a newer one.
	renderMu sync.Mutex
	// mu guards tabs. Join, Update and Leave are called under it, so a
	// user's first and last tab cannot race; the tracker's callbacks and
	// the middleware never take it.
	mu      sync.Mutex
	tabs    map[string]*presenceTab // connection id → tab
	leaving sync.Map                // user id → true while the app removes them
	log     obsLog
}

func newPresenceLab(o *ops) *presenceLab {
	l := &presenceLab{o: o, broker: o.broker(tavern.WithBufferSize(16)), tabs: map[string]*presenceTab{}}
	l.tracker = presence.New(l.broker, presence.Config{
		StaleTimeout:        prStale,
		PresenceTopicSuffix: prSuffix,
		TargetID:            prTarget,
		RenderFunc:          renderPeople,
		OnJoin:              func(_ string, info presence.Info) { l.log.add("%s joined", info.Name) },
		OnUpdate: func(_ string, info presence.Info) {
			l.log.add("%s updated: status %v, %v tabs", info.Name, info.Metadata["status"], info.Metadata["tabs"])
		},
		OnLeave: func(_ string, info presence.Info) {
			why := "stale: no heartbeat for 2 s"
			if _, ok := l.leaving.LoadAndDelete(info.UserID); ok {
				why = "left: last tab closed"
			}
			l.log.add("%s removed (%s)", info.Name, why)
		},
	})
	// The tracker renders after releasing its lock, so two changes could
	// publish their renders out of order. Rendering again here, in order,
	// from the current list fixes that.
	l.broker.UseTopics(prRoom+prSuffix, func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, _ string) {
			l.renderMu.Lock()
			defer l.renderMu.Unlock()
			next(topic, l.peopleFragment())
		}
	})
	return l
}

func (l *presenceLab) peopleFragment() string {
	return tavern.RenderFragments(tavern.Replace(prTarget, renderPeople(prRoom, l.tracker.List(prRoom))))
}

// renderPeople lists users by ID, with their status and tab count.
func renderPeople(_ string, users []presence.Info) string {
	sort.Slice(users, func(i, j int) bool { return users[i].UserID < users[j].UserID })
	var b strings.Builder
	b.WriteString(`<ul id="people">`)
	for _, u := range users {
		fmt.Fprintf(&b, `<li data-user=%q data-status="%v" data-tabs="%v">%s: %v, %v tab(s)</li>`,
			u.UserID, u.Metadata["status"], u.Metadata["tabs"], template.HTMLEscapeString(u.Name), u.Metadata["status"], u.Metadata["tabs"])
	}
	if len(users) == 0 {
		b.WriteString(`<li data-empty>Nobody is here.</li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

func (l *presenceLab) userTabsLocked(user string) int {
	n := 0
	for _, t := range l.tabs {
		if t.user == user {
			n++
		}
	}
	return n
}

// arrive records a tab and joins its user, or counts the extra tab.
func (l *presenceLab) arrive(conn string, p *persona) *presenceTab {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := &presenceTab{user: p.id}
	l.tabs[conn] = t
	l.joinOrCountLocked(p.id)
	return t
}

func (l *presenceLab) joinOrCountLocked(user string) {
	n := l.userTabsLocked(user)
	if _, ok := l.tracker.Get(prRoom, user); !ok {
		p, _ := personaByID(user)
		l.tracker.Join(prRoom, presence.Info{UserID: user, Name: p.name, Metadata: map[string]any{"status": "here", "tabs": n}})
		return
	}
	l.tracker.Update(prRoom, user, map[string]any{"tabs": n})
}

// depart forgets a tab and leaves its user if it was the last one.
func (l *presenceLab) depart(conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tabs[conn]
	if !ok {
		return
	}
	delete(l.tabs, conn)
	if n := l.userTabsLocked(t.user); n > 0 {
		l.tracker.Update(prRoom, t.user, map[string]any{"tabs": n})
		return
	}
	l.leaving.Store(t.user, true)
	l.tracker.Leave(prRoom, t.user)
	l.leaving.Delete(t.user)
}

// heartbeat keeps the tab's user present while the stream is open. A user
// removed as stale while this tab was paused joins again when it resumes.
func (l *presenceLab) heartbeat(ctx context.Context, conn string, t *presenceTab) {
	for sleepCtx(ctx, prHeartbeat) {
		if t.paused.Load() {
			continue
		}
		l.mu.Lock()
		if l.tabs[conn] == t {
			if _, ok := l.tracker.Get(prRoom, t.user); ok {
				l.tracker.Heartbeat(prRoom, t.user)
			} else {
				l.joinOrCountLocked(t.user)
			}
		}
		l.mu.Unlock()
	}
}

func (l *presenceLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), snapshot string, err error) {
	l.renderMu.Lock()
	defer l.renderMu.Unlock()
	snapshot = tavern.NewSSEMessage("oob", l.peopleFragment()).String()
	msgs, unsubscribe, err = openAll(func() (keyedSub, error) {
		ch, unsub := l.broker.SubscribeWith(prRoom+prSuffix, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn}))
		return subOrErr("people", ch, unsub)
	})
	return msgs, unsubscribe, snapshot, err
}

func (l *presenceLab) state() map[string]any {
	l.mu.Lock()
	tabs := map[string]int{}
	paused := map[string]int{}
	for _, t := range l.tabs {
		tabs[t.user]++
		if t.paused.Load() {
			paused[t.user]++
		}
	}
	l.mu.Unlock()
	var people []map[string]any
	for _, u := range l.tracker.List(prRoom) {
		people = append(people, map[string]any{"user": u.UserID, "name": u.Name, "status": u.Metadata["status"], "tabs": u.Metadata["tabs"], "lastSeen": u.LastSeen.Format("15:04:05.000")})
	}
	sort.Slice(people, func(i, j int) bool { return people[i]["user"].(string) < people[j]["user"].(string) })
	return map[string]any{"present": people, "openTabs": tabs, "pausedTabs": paused, "observations": l.log.list()}
}

func (a *App) registerPresence() {
	l := a.ops.presence
	a.mux.HandleFunc("GET /showcase/ops/presence", a.withPersona(func(w http.ResponseWriter, _ *http.Request, p *persona) {
		a.renderOpsPage(w, presencePage, map[string]any{"Title": "Presence across tabs", "Family": "presence", "P": p, "Statuses": prStatuses})
	}))
	a.mux.HandleFunc("GET /showcase/ops/presence/events", a.withPersona(func(w http.ResponseWriter, r *http.Request, p *persona) {
		a.serveOwned(w, r, ownedStream{
			reg: &l.reg,
			openSnapshot: func(conn string) (<-chan tavern.TopicMessage, func(), string, error) {
				ch, unsub, snap, err := l.open(conn)
				if err != nil {
					return nil, nil, "", err
				}
				// The tab counts only once it is subscribed, so a refused
				// stream never makes its user present.
				t := l.arrive(conn, p)
				ctx, stop := context.WithCancel(r.Context())
				hb := make(chan struct{})
				go func() { defer close(hb); l.heartbeat(ctx, conn, t) }()
				return ch, func() {
					stop()
					<-hb
					unsub()
					l.depart(conn)
				}, snap, nil
			},
		})
	}))
	owned := func(h func(w http.ResponseWriter, r *http.Request, t *presenceTab)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			l.mu.Lock()
			t := l.tabs[r.URL.Query().Get("conn")]
			l.mu.Unlock()
			if t == nil {
				http.Error(w, "This tab is not in the room", http.StatusNotFound)
				return
			}
			h(w, r, t)
		}
	}
	a.mux.HandleFunc("POST /showcase/ops/presence/status", owned(func(w http.ResponseWriter, r *http.Request, t *presenceTab) {
		v := r.URL.Query().Get("value")
		if !slices.Contains(prStatuses, v) {
			http.Error(w, "Unknown status", http.StatusBadRequest)
			return
		}
		l.tracker.Update(prRoom, t.user, map[string]any{"status": v})
		w.WriteHeader(http.StatusNoContent)
	}))
	a.mux.HandleFunc("POST /showcase/ops/presence/heartbeats", owned(func(w http.ResponseWriter, r *http.Request, t *presenceTab) {
		t.paused.Store(r.URL.Query().Get("on") != "1")
		w.WriteHeader(http.StatusNoContent)
	}))
	// leave replaces the tab's region, so htmx closes its connection; the
	// stream then ends as it does when a tab is closed.
	a.mux.HandleFunc("POST /showcase/ops/presence/leave", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<p id="presence-left">You left the room. <a href="">Join again</a></p>`)
	})
	a.mux.HandleFunc("GET /showcase/ops/presence/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, l.state())
	})
}

var presencePage = deliveryPage("presence", `{{define "main"}}
<p class="note">You are {{.P.Name}}. Open this page again in another tab, as {{.P.Name}} or as the other person. Presence is per person: a second tab does not add a second entry, closing one tab keeps you here while another is open, and the status is shared by your tabs. A tab that stops sending heartbeats is not noticed while another of your tabs still sends them; with none, you are removed within about two seconds and join again when heartbeats resume.</p>
<p><a href="/showcase/ops/presence?as=alice">As Alice</a> · <a href="/showcase/ops/presence?as=bob">As Bob</a></p>
<div id="presence-tab">
<section id="presence-region" sse-connect="/showcase/ops/presence/events?as={{.P.ID}}&amp;conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <h2>In the room</h2>
  <ul id="people"><li>…</li></ul>
</section>
<p>
  {{range .Statuses}}<button hx-post="/showcase/ops/presence/status?conn={{$.Conn}}&amp;value={{.}}" hx-swap="none">Status: {{.}}</button> {{end}}
</p>
<p>
  <button hx-post="/showcase/ops/presence/heartbeats?conn={{.Conn}}&amp;on=0" hx-swap="none">Stop this tab's heartbeats</button>
  <button hx-post="/showcase/ops/presence/heartbeats?conn={{.Conn}}&amp;on=1" hx-swap="none">Resume them</button>
  <button hx-post="/showcase/ops/presence/leave?conn={{.Conn}}" hx-target="#presence-tab" hx-swap="innerHTML">Leave the room</button>
</p>
</div>
{{end}}`)
