package main

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/catgoose/tavern"
)

// collabStreams tracks open collaboration streams so the demo can end one
// and hold its reconnect.
type collabStreams struct {
	streams sync.Map // connection id → *droppableStream, while its stream is open
	holds   sync.Map // connection id → chan struct{}, closed on resume
}

func (a *App) registerCollab() {
	a.collab = newCollab(a.broker)
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
		feed.ServeHTTP(w, r)
	}))
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
	if v, held := a.collabStreams.holds.Load(conn); held {
		select {
		case <-v.(chan struct{}):
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

// collabDrop ends a window's stream and holds its reconnect until resume.
func (a *App) collabDrop(w http.ResponseWriter, r *http.Request) {
	conn := r.URL.Query().Get("conn")
	v, ok := a.collabStreams.streams.Load(conn)
	if !ok {
		http.Error(w, "This tab has no live connection", http.StatusNotFound)
		return
	}
	cs := v.(*droppableStream)
	a.collabStreams.holds.LoadOrStore(conn, make(chan struct{}))
	cs.unsubscribe()
	select {
	case <-cs.done:
	case <-r.Context().Done():
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) collabResume(w http.ResponseWriter, r *http.Request) {
	v, ok := a.collabStreams.holds.LoadAndDelete(r.URL.Query().Get("conn"))
	if !ok {
		http.Error(w, "This tab is not being held", http.StatusNotFound)
		return
	}
	close(v.(chan struct{}))
	w.WriteHeader(http.StatusNoContent)
}

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
