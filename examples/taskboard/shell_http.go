package main

import (
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"

	"github.com/catgoose/tavern"
)

func (a *App) registerShell() {
	a.shell = newShell(a.broker, a.done)
	ops := a.broker.GroupHandler(shellOpsGroup, tavern.WithWriteTimeout(a.cfg.WriteTimeout))

	a.mux.HandleFunc("GET /showcase/shell", a.shellPage)
	a.mux.HandleFunc("GET /showcase/shell/lifeline", a.withPersona(a.shellLifeline))
	a.mux.HandleFunc("GET /showcase/shell/views/{view}", a.withPersona(a.shellView))
	a.mux.HandleFunc("GET /showcase/shell/views/board/events", a.withPersona(a.shellBoardEvents))
	a.mux.HandleFunc("GET /showcase/shell/views/feeds/events", a.withPersona(a.shellFeedEvents))
	a.mux.Handle("GET /showcase/shell/views/ops/events", a.withPersona(func(w http.ResponseWriter, r *http.Request, _ *persona) {
		ops.ServeHTTP(w, r)
	}))
	a.mux.HandleFunc("GET /showcase/shell/diagnostics", a.withPersona(a.shellDiagnostics))

	a.mux.HandleFunc("POST /showcase/shell/notices", a.withPersona(func(w http.ResponseWriter, _ *http.Request, p *persona) {
		a.shell.postNotice(p)
		w.WriteHeader(http.StatusNoContent)
	}))
	a.mux.HandleFunc("POST /showcase/shell/follow", a.withPersona(a.shellFollow))
	a.mux.HandleFunc("POST /showcase/shell/leads", a.withPersona(a.shellLeads))
	a.mux.HandleFunc("POST /showcase/shell/channels/{channel}", a.withPersona(func(w http.ResponseWriter, r *http.Request, p *persona) {
		respond(w, a.shell.postChannel(p, r.PathValue("channel")))
	}))
	a.mux.HandleFunc("POST /showcase/shell/role-note", a.withPersona(a.shellRoleNote))
	a.mux.HandleFunc("POST /showcase/shell/board/tasks", a.withPersona(func(w http.ResponseWriter, _ *http.Request, p *persona) {
		a.shell.addBoardTask(p)
		w.WriteHeader(http.StatusNoContent)
	}))
	a.mux.HandleFunc("POST /showcase/shell/feeds/{feed}", a.withPersona(func(w http.ResponseWriter, r *http.Request, p *persona) {
		respond(w, a.shell.publishFeed(p, r.PathValue("feed")))
	}))
	a.mux.HandleFunc("POST /showcase/shell/ops/{kind}", a.withPersona(func(w http.ResponseWriter, r *http.Request, _ *persona) {
		respond(w, a.shell.bumpOps(r.PathValue("kind")))
	}))
	a.mux.HandleFunc("POST /showcase/shell/views/board/break", a.withPersona(a.shellBreakBoard))
	a.mux.HandleFunc("POST /showcase/shell/views/board/recover", a.withPersona(a.shellRecoverBoard))
}

func (a *App) shellLifeline(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	ch, unsub, err := a.shell.openLifeline(p, conn)
	if errors.Is(err, errAdmission) {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		respond(w, err)
		return
	}
	defer unsub()
	err = tavern.StreamSSE(r.Context(), w, ch, shellFrame,
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	a.logger.Info("shell lifeline ended", "persona", p.id, "write_error", err)
}

// shellBoardEvents streams the board view. While the demo holds this
// window's board, a reconnect waits.
func (a *App) shellBoardEvents(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	if a.shell.owner(p, conn) != nil {
		http.Error(w, "Open the shell first", http.StatusNotFound)
		return
	}
	if h := a.shell.holds.get(conn); h != nil {
		select {
		case <-h.released:
		case <-r.Context().Done():
			return
		case <-a.done:
			return
		}
	}
	status := tavern.NewSubscriptionStatus()
	ch, unsub := a.shell.openBoard(status)
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	cs := &droppableStream{unsubscribe: unsub, done: make(chan struct{})}
	a.shell.boardStreams.Store(conn, cs)
	defer func() {
		unsub()
		a.shell.boardStreams.CompareAndDelete(conn, cs)
		close(cs.done)
	}()
	err := tavern.StreamSSE(r.Context(), w, ch, shellFrame,
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	unsub()
	a.logger.Info("shell board stream ended", "reason", status.Termination().Reason.String(), "write_error", err)
}

// shellFeedEvents streams every topic under shell/feeds/. The view keeps the
// newest shellFeedLimit items, numbered per stream.
func (a *App) shellFeedEvents(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	if a.shell.owner(p, conn) != nil {
		http.Error(w, "Open the shell first", http.StatusNotFound)
		return
	}
	ch, unsub := a.shell.broker.SubscribeGlobWith(shellFeedPattern, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn + "/feeds"}))
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer unsub()
	n := 0
	encode := func(m tavern.TopicMessage) string {
		n++
		frags := []tavern.Fragment{tavern.Prepend("shell-feed-list", renderShell("feed-item", feedItem{ID: feedItemID(n), Topic: m.Topic, Text: m.Data}))}
		if n > shellFeedLimit {
			frags = append(frags, tavern.Delete(feedItemID(n-shellFeedLimit)))
		}
		return tavern.NewSSEMessage("shell-feed", tavern.RenderFragments(frags...)).String()
	}
	err := tavern.StreamSSE(r.Context(), w, ch, encode,
		tavern.WithStreamSnapshot(func() string {
			return tavern.NewSSEMessage("shell-feed-ready", tavern.RenderFragments(tavern.Replace("shell-feed-list", ""))).String()
		}),
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	a.logger.Info("shell feed stream ended", "write_error", err)
}

type feedItem struct {
	ID, Topic, Text string
}

func feedItemID(n int) string { return "shell-feed-item-" + strconv.Itoa(n) }

func (a *App) shellView(w http.ResponseWriter, r *http.Request, p *persona) {
	view := r.PathValue("view")
	if !slices.Contains(shellViews, view) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := shellFragments.ExecuteTemplate(w, "view-"+view, map[string]any{"P": p, "Conn": r.URL.Query().Get("conn")}); err != nil {
		a.logger.Error("render shell view", "view", view, "err", err)
	}
}

func (a *App) shellFollow(w http.ResponseWriter, r *http.Request, p *persona) {
	q := r.URL.Query()
	changed, err := a.shell.follow(p, q.Get("conn"), q.Get("channel"), q.Get("on") == "1")
	if err != nil {
		respond(w, err)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusOK) // already in that state
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) shellLeads(w http.ResponseWriter, r *http.Request, p *persona) {
	if _, err := a.shell.leadsChannel(p, r.URL.Query().Get("on") == "1"); err != nil {
		respond(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) shellRoleNote(w http.ResponseWriter, r *http.Request, p *persona) {
	var to *role
	for _, rl := range roles {
		if rl.name == r.URL.Query().Get("to") {
			to = rl
		}
	}
	if to == nil {
		http.Error(w, "Unknown role", http.StatusBadRequest)
		return
	}
	a.shell.notifyRole(p, to)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) shellDiagnostics(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	if a.shell.owner(p, conn) != nil {
		http.Error(w, "No such window", http.StatusNotFound)
		return
	}
	writeJSON(w, a.shell.diagnostics(conn))
}

// shellBreakBoard ends this window's board stream and holds its reconnect,
// as a lost connection would, until recover or expiry.
func (a *App) shellBreakBoard(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	v, ok := a.shell.boardStreams.Load(conn)
	if !ok || a.shell.owner(p, conn) != nil {
		http.Error(w, "This window has no board stream", http.StatusNotFound)
		return
	}
	cs := v.(*droppableStream)
	if _, err := a.shell.holds.add(conn); err != nil {
		http.Error(w, "Cannot break the stream now: "+err.Error(), http.StatusServiceUnavailable)
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

func (a *App) shellRecoverBoard(w http.ResponseWriter, r *http.Request, p *persona) {
	conn := r.URL.Query().Get("conn")
	if a.shell.owner(p, conn) != nil || !a.shell.holds.take(conn) {
		http.Error(w, "This window's board is not being held", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) shellPage(w http.ResponseWriter, r *http.Request) {
	p, ok := personaByID(r.URL.Query().Get("as"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		if err := shellChooserTmpl.Execute(w, personas); err != nil {
			a.logger.Error("render shell chooser", "err", err)
		}
		return
	}
	err := shellPageTmpl.Execute(w, map[string]any{"P": p, "Conn": newConnID(), "Lead": p.role.seesInternal})
	if err != nil {
		a.logger.Error("render shell page", "err", err)
	}
}

var (
	shellPageTmpl    = template.Must(template.New("shell").Parse(shellPageHTML))
	shellChooserTmpl = template.Must(template.New("shell-chooser").Parse(shellChooserHTML))
)
