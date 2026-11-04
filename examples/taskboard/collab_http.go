package main

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// collabStreams tracks open collaboration streams for the demo controls.
type collabStreams struct {
	streams     sync.Map // connection id → *droppableStream, while its stream is open
	feedPausers sync.Map // connection id → *pauser, while its feed stream is open
	holds       holds
}

// Reconnect holds last at most holdTTL and at most holdMax exist at once,
// so windows that disconnect and never come back leave nothing behind.
const (
	holdTTL = time.Minute
	holdMax = 32
)

var (
	errHoldsFull   = errors.New("too many windows are held")
	errHoldsClosed = errors.New("shutting down")
)

// holds keeps a window's reconnect waiting after the demo disconnects it.
type holds struct {
	mu     sync.Mutex
	ttl    time.Duration
	max    int
	m      map[string]*hold
	closed bool
}

// hold is released once: on resume, on expiry or at shutdown.
type hold struct {
	released chan struct{}
	once     sync.Once
	timer    *time.Timer
}

func (h *hold) release() {
	h.once.Do(func() {
		h.timer.Stop()
		close(h.released)
	})
}

// add holds conn's reconnects, or returns its existing hold.
func (hs *holds) add(conn string) (*hold, error) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.closed {
		return nil, errHoldsClosed
	}
	if h, ok := hs.m[conn]; ok {
		return h, nil
	}
	if len(hs.m) >= hs.max {
		return nil, errHoldsFull
	}
	if hs.m == nil {
		hs.m = map[string]*hold{}
	}
	h := &hold{released: make(chan struct{})}
	h.timer = time.AfterFunc(hs.ttl, func() { hs.remove(conn, h) })
	hs.m[conn] = h
	return h, nil
}

// remove releases h and forgets it, unless conn has since been held anew.
func (hs *holds) remove(conn string, h *hold) {
	hs.mu.Lock()
	if hs.m[conn] == h {
		delete(hs.m, conn)
	}
	hs.mu.Unlock()
	h.release()
}

func (hs *holds) get(conn string) *hold {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.m[conn]
}

// take releases and forgets conn's hold.
func (hs *holds) take(conn string) bool {
	hs.mu.Lock()
	h, ok := hs.m[conn]
	delete(hs.m, conn)
	hs.mu.Unlock()
	if ok {
		h.release()
	}
	return ok
}

func (hs *holds) len() int {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return len(hs.m)
}

// close releases every hold and refuses new ones.
func (hs *holds) close() {
	hs.mu.Lock()
	all := hs.m
	hs.m, hs.closed = nil, true
	hs.mu.Unlock()
	for _, h := range all {
		h.release()
	}
}

func (a *App) registerCollab() {
	a.collab = newCollab(a.broker)
	a.collabStreams.holds.ttl, a.collabStreams.holds.max = holdTTL, holdMax
	a.broker.DynamicGroup(groupFeed, func(r *http.Request) []string {
		p, ok := personaByID(r.FormValue("as"))
		if !ok {
			return nil
		}
		return feedTopics(p)
	})
	feed := a.broker.DynamicGroupHandler(groupFeed, tavern.WithWriteTimeout(a.cfg.WriteTimeout))

	a.mux.HandleFunc("GET /showcase/collaboration", a.collabPage)
	a.mux.HandleFunc("GET /showcase/collaboration/events", a.withPersona(a.collabEvents))
	a.mux.Handle("GET /showcase/collaboration/feed", a.withPersona(func(w http.ResponseWriter, r *http.Request, _ *persona) {
		conn := r.URL.Query().Get("conn")
		gate := &pauser{resume: make(chan struct{})}
		a.collabStreams.feedPausers.Store(conn, gate)
		defer a.collabStreams.feedPausers.CompareAndDelete(conn, gate)
		feed.ServeHTTP(pausableWriter{w, func() { gate.wait(r.Context(), a.done) }}, r)
	}))
	a.mux.HandleFunc("POST /showcase/collaboration/feed/pause", a.feedPaused(true))
	a.mux.HandleFunc("POST /showcase/collaboration/feed/resume", a.feedPaused(false))
	a.mux.HandleFunc("POST /showcase/collaboration/tasks", a.withPersona(a.collabCreate))
	a.mux.HandleFunc("GET /showcase/collaboration/tasks/{id}/edit", a.withPersona(withTask(a.collabEditor)))
	a.mux.HandleFunc("POST /showcase/collaboration/tasks/{id}/title", a.withPersona(withTask(a.collabRename)))
	a.mux.HandleFunc("POST /showcase/collaboration/tasks/{id}/move", a.withPersona(withTask(a.collabMove)))
	a.mux.HandleFunc("POST /showcase/collaboration/tasks/{id}/assign", a.withPersona(withTask(a.collabAssign)))
	a.mux.HandleFunc("POST /showcase/collaboration/tasks/{id}/delete", a.withPersona(withTask(a.collabDelete)))
	a.mux.HandleFunc("POST /showcase/collaboration/clear-done", a.withPersona(a.collabClearDone))
	a.mux.HandleFunc("POST /showcase/collaboration/drop", a.collabDrop)
	a.mux.HandleFunc("POST /showcase/collaboration/resume", a.collabResume)
}

// withPersona resolves the demo persona named by the "as" parameter. Only
// the persona name comes from the request; what it may see and do comes from
// its fixed definition.
func (a *App) withPersona(h func(http.ResponseWriter, *http.Request, *persona)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := personaByID(r.FormValue("as"))
		if !ok {
			http.Error(w, "Choose Alice or Bob", http.StatusBadRequest)
			return
		}
		h(w, r, p)
	}
}

// collabEvents streams the persona's board changes and inbox. If the demo
// is holding this window's reconnect, it waits first.
func (a *App) collabEvents(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	if h := a.collabStreams.holds.get(conn); h != nil {
		// Leaving early does not end the hold: the browser's next attempt
		// waits again until resume, expiry or shutdown.
		select {
		case <-h.released:
		case <-r.Context().Done():
			return
		case <-a.done:
			return
		}
	}

	status := tavern.NewSubscriptionStatus()
	ch, unsub := a.collab.subscribe(p, status)
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	cs := &droppableStream{unsubscribe: unsub, done: make(chan struct{})}
	a.collabStreams.streams.Store(conn, cs)
	defer func() {
		unsub()
		a.collabStreams.streams.CompareAndDelete(conn, cs)
		close(cs.done)
	}()

	err := tavern.StreamSSE(r.Context(), w, ch, collabFrame,
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	unsub()
	a.logger.Info("collaboration stream ended", "persona", p.id, "reason", status.Termination().Reason.String(), "write_error", err)
}

// collabFrame turns a message into an SSE frame. Notices are already
// frames; board changes and snapshots are raw OOB HTML, sent as "board"
// events. A batch of board changes is one message, so it becomes one event.
func collabFrame(msg string) string {
	if strings.HasPrefix(msg, "event: ") {
		return msg
	}
	return tavern.NewSSEMessage("board", msg).String()
}

// collabDrop ends a window's stream and holds its reconnect until resume
// or expiry.
func (a *App) collabDrop(w http.ResponseWriter, r *http.Request) {
	conn := r.URL.Query().Get("conn")
	v, ok := a.collabStreams.streams.Load(conn)
	if !ok {
		http.Error(w, "This tab has no live connection", http.StatusNotFound)
		return
	}
	cs := v.(*droppableStream)
	if _, err := a.collabStreams.holds.add(conn); err != nil {
		http.Error(w, "Cannot disconnect now: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	cs.unsubscribe()
	select {
	case <-cs.done:
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) collabResume(w http.ResponseWriter, r *http.Request) {
	if !a.collabStreams.holds.take(r.URL.Query().Get("conn")) {
		http.Error(w, "This tab is not being held", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// feedPaused pauses or resumes writes on a window's feed stream, so its
// queue really fills and updates are dropped.
func (a *App) feedPaused(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := a.collabStreams.feedPausers.Load(r.URL.Query().Get("conn"))
		if !ok {
			http.Error(w, "This tab has no live feed", http.StatusNotFound)
			return
		}
		v.(*pauser).set(paused)
		w.WriteHeader(http.StatusNoContent)
	}
}

// pausableWriter waits before each write. Unwrap lets the stream reach the
// underlying writer's flush and write deadlines.
type pausableWriter struct {
	http.ResponseWriter
	wait func()
}

func (pw pausableWriter) Write(b []byte) (int, error) {
	pw.wait()
	return pw.ResponseWriter.Write(b)
}

func (pw pausableWriter) Unwrap() http.ResponseWriter { return pw.ResponseWriter }

func taskID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	return id, err == nil && id > 0
}

// respond maps a model error to a status code.
func respond(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errInvalid):
		http.Error(w, strings.TrimPrefix(err.Error(), errInvalid.Error()+": "), http.StatusBadRequest)
	case errors.Is(err, errConflict):
		http.Error(w, strings.TrimPrefix(err.Error(), errConflict.Error()+": "), http.StatusConflict)
	case errors.Is(err, errForbidden):
		http.Error(w, strings.TrimPrefix(err.Error(), errForbidden.Error()+": "), http.StatusForbidden)
	case errors.Is(err, errNotFound):
		http.Error(w, "That task no longer exists", http.StatusNotFound)
	default:
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
	}
}

// withTask parses the task ID from the path.
func withTask(h func(http.ResponseWriter, *http.Request, *persona, int)) func(http.ResponseWriter, *http.Request, *persona) {
	return func(w http.ResponseWriter, r *http.Request, p *persona) {
		id, ok := taskID(r)
		if !ok {
			http.Error(w, "Unknown task", http.StatusBadRequest)
			return
		}
		h(w, r, p, id)
	}
}

func (a *App) collabCreate(w http.ResponseWriter, r *http.Request, p *persona) {
	respond(w, a.collab.create(p, r.FormValue("title"), r.FormValue("internal") == "on"))
}

func (a *App) collabRename(w http.ResponseWriter, r *http.Request, p *persona, id int) {
	if err := a.collab.rename(p, id, r.FormValue("title")); err != nil {
		respond(w, err)
		return
	}
	a.renderCollab(w, "editor-closed", nil)
}

func (a *App) collabMove(w http.ResponseWriter, r *http.Request, p *persona, id int) {
	respond(w, a.collab.move(p, id, r.FormValue("to")))
}

func (a *App) collabAssign(w http.ResponseWriter, r *http.Request, p *persona, id int) {
	var to *persona
	if name := r.FormValue("to"); name != "" {
		var ok bool
		if to, ok = personaByID(name); !ok {
			http.Error(w, "Unknown person", http.StatusBadRequest)
			return
		}
	}
	respond(w, a.collab.assign(p, id, to))
}

func (a *App) collabDelete(w http.ResponseWriter, _ *http.Request, p *persona, id int) {
	respond(w, a.collab.remove(p, id))
}

func (a *App) collabClearDone(w http.ResponseWriter, _ *http.Request, p *persona) {
	a.collab.clearDone(p)
	w.WriteHeader(http.StatusNoContent)
}

// collabEditor renders the edit form outside the board, so live changes to
// the card do not replace a title being typed.
func (a *App) collabEditor(w http.ResponseWriter, _ *http.Request, p *persona, id int) {
	title, err := a.collab.title(p, id)
	if err != nil {
		respond(w, err)
		return
	}
	a.renderCollab(w, "editor", map[string]any{"ID": id, "Title": title})
}

func (a *App) collabPage(w http.ResponseWriter, r *http.Request) {
	p, ok := personaByID(r.URL.Query().Get("as"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		if err := collabChooserTmpl.Execute(w, personas); err != nil {
			a.logger.Error("render persona chooser", "err", err)
		}
		return
	}
	var other *persona
	for _, q := range personas {
		if q != p {
			other = q
		}
	}
	feed := feedTopics(p)
	err := collabPageTmpl.Execute(w, map[string]any{
		"P": p, "Other": other, "Conn": newConnID(), "Columns": columns, "Titles": columnTitles,
		"FeedEvents": strings.Join(feed, ","),
	})
	if err != nil {
		a.logger.Error("render collaboration page", "err", err)
	}
}

func (a *App) renderCollab(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := collabFragments.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Error("render collaboration fragment", "name", name, "err", err)
	}
}

var (
	collabPageTmpl    = template.Must(template.New("collab").Parse(collabPageHTML))
	collabChooserTmpl = template.Must(template.New("chooser").Parse(collabChooserHTML))
)
