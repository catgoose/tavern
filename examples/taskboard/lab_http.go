package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/catgoose/tavern"
)

// labCommandMax bounds a command body. tavern-js sends command-* attributes
// as a JSON object of strings.
const labCommandMax = 1 << 10

func (a *App) registerLab() {
	a.lab = newLab(a.broker, a.done)
	a.mux.HandleFunc("GET /showcase/lab", a.labPage)
	a.mux.HandleFunc("GET /showcase/lab/{region}/events", a.withPersona(a.labEvents))
	a.mux.HandleFunc("POST /showcase/lab/tasks", a.withPersona(a.labAdd))
	a.mux.HandleFunc("GET /showcase/lab/tasks/{id}/edit", a.withPersona(withTask(a.labEditor)))
	a.mux.HandleFunc("GET /showcase/lab/tasks/{id}/view", a.withPersona(withTask(a.labCard)))
	a.mux.HandleFunc("POST /showcase/lab/tasks/{id}/title", a.withPersona(withTask(a.labRename)))
	a.mux.HandleFunc("POST /showcase/lab/tasks/{id}/delete", a.withPersona(withTask(a.labDelete)))

	// tavern-js commands: JSON bodies.
	a.mux.HandleFunc("POST /showcase/lab/tasks/{id}/vote", a.withPersona(withTask(a.labVote)))
	a.mux.HandleFunc("POST /showcase/lab/burst", a.withPersona(a.labBurst))
	a.mux.HandleFunc("POST /showcase/lab/log/clear", a.withPersona(a.labClearLog))
	a.mux.HandleFunc("POST /showcase/lab/resync", a.withPersona(a.labResync))
}

// decodeCommand reads a tavern-js command body into dst. The whole body
// must fit in labCommandMax (413 otherwise) and be one JSON object with no
// unknown fields, optionally followed by whitespace (400 otherwise). Other
// content types get 415.
func decodeCommand(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		http.Error(w, "Send the command as JSON", http.StatusUnsupportedMediaType)
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, labCommandMax))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "Command too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Could not read the command", http.StatusBadRequest)
		}
		return false
	}
	trimmed := bytes.TrimSpace(body)
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if len(trimmed) == 0 || trimmed[0] != '{' || dec.Decode(dst) != nil || dec.InputOffset() != int64(len(trimmed)) {
		http.Error(w, "Malformed command", http.StatusBadRequest)
		return false
	}
	return true
}

func (a *App) labEvents(w http.ResponseWriter, r *http.Request, p *persona) {
	region := r.PathValue("region")
	if !slices.Contains(labRegions, region) {
		http.NotFound(w, r)
		return
	}
	status := tavern.NewSubscriptionStatus()
	ch, unsub := a.lab.subscribe(p, region, r.URL.Query().Get("conn"), status)
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer unsub()
	err := tavern.StreamSSE(r.Context(), w, ch, func(msg string) string { return msg },
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	unsub()
	a.logger.Info("lab stream ended", "region", region, "persona", p.id, "reason", status.Termination().Reason.String(), "write_error", err)
}

func (a *App) labVote(w http.ResponseWriter, r *http.Request, p *persona, id int) {
	var cmd struct {
		ID string `json:"id"`
	}
	if !decodeCommand(w, r, &cmd) {
		return
	}
	if cmd.ID != strconv.Itoa(id) {
		http.Error(w, "The command names a different task", http.StatusBadRequest)
		return
	}
	respond(w, a.lab.vote(p, id))
}

func (a *App) labBurst(w http.ResponseWriter, r *http.Request, p *persona) {
	var cmd struct {
		Count string `json:"count"`
	}
	if !decodeCommand(w, r, &cmd) {
		return
	}
	n, err := strconv.Atoi(cmd.Count)
	if err != nil {
		http.Error(w, "count must be a number", http.StatusBadRequest)
		return
	}
	respond(w, a.lab.startBurst(p, n))
}

func (a *App) labClearLog(w http.ResponseWriter, r *http.Request, p *persona) {
	var cmd struct{}
	if !decodeCommand(w, r, &cmd) {
		return
	}
	a.lab.clearLog(p)
	w.WriteHeader(http.StatusNoContent)
}

// labResync asks for a fresh snapshot on one of this window's streams. The
// connection must belong to the persona making the request.
func (a *App) labResync(w http.ResponseWriter, r *http.Request, p *persona) {
	var cmd struct{}
	if !decodeCommand(w, r, &cmd) {
		return
	}
	q := r.URL.Query()
	if err := a.lab.resync(p, q.Get("conn"), q.Get("region")); err != nil {
		http.Error(w, "This window has no such stream", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) labAdd(w http.ResponseWriter, r *http.Request, p *persona) {
	respond(w, a.lab.add(p, r.FormValue("title")))
}

func (a *App) labDelete(w http.ResponseWriter, _ *http.Request, p *persona, id int) {
	if err := a.lab.remove(p, id); err != nil {
		respond(w, err)
		return
	}
	// The card is replaced by an empty placeholder with its ID, which takes
	// focus out of the region so its updates resume. The delete fragment the
	// removal publishes then removes the placeholder; if that fragment came
	// first it was held while the region had focus, and the catch-up
	// snapshot replaces the list. Either way the delete has its target.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div id="lab-edit-%d" hidden></div>`, id)
}

// labEditor opens the editor with the title revision it starts from.
func (a *App) labEditor(w http.ResponseWriter, _ *http.Request, p *persona, id int) {
	t, err := a.lab.task(p, id)
	if err != nil {
		respond(w, err)
		return
	}
	a.renderLabFragment(w, http.StatusOK, "editor", editorView{T: t, P: p, Draft: t.Title, Rev: t.TitleRev})
}

func (a *App) labCard(w http.ResponseWriter, _ *http.Request, p *persona, id int) {
	t, err := a.lab.task(p, id)
	if err != nil {
		respond(w, err)
		return
	}
	a.renderLabFragment(w, http.StatusOK, "edit-body", labView{T: t, P: p})
}

// labRename saves an edit. Every failure re-renders the editor with the
// draft as typed: a conflict shows the current title and offers to save
// over it, a deleted task cannot be saved, and an invalid title says why.
func (a *App) labRename(w http.ResponseWriter, r *http.Request, p *persona, id int) {
	draft := r.FormValue("title")
	rev, err := strconv.Atoi(r.FormValue("rev"))
	if err != nil {
		http.Error(w, "Missing title revision", http.StatusBadRequest)
		return
	}
	t, err := a.lab.rename(p, id, rev, draft)
	view := editorView{P: p, Draft: draft, Rev: rev, ID: id}
	switch {
	case err == nil:
		a.renderLabFragment(w, http.StatusOK, "edit-body", labView{T: t, P: p})
	case errors.Is(err, errStale):
		view.T, view.Rev, view.Conflict = t, t.TitleRev, true
		a.renderLabFragment(w, http.StatusConflict, "editor", view)
	case errors.Is(err, errNotFound):
		view.Deleted = true
		a.renderLabFragment(w, http.StatusNotFound, "editor", view)
	case errors.Is(err, errInvalid):
		view.T, view.Problem = &labTask{ID: id}, strings.TrimPrefix(err.Error(), errInvalid.Error()+": ")
		a.renderLabFragment(w, http.StatusUnprocessableEntity, "editor", view)
	default:
		respond(w, err)
	}
}

// editorView renders the editor. T is the server's current task, if any.
type editorView struct {
	T        *labTask
	P        *persona
	ID       int
	Draft    string
	Rev      int
	Conflict bool
	Deleted  bool
	Problem  string
}

func (v editorView) TaskID() int {
	if v.T != nil && v.T.ID != 0 {
		return v.T.ID
	}
	return v.ID
}

func (a *App) renderLabFragment(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := labFragments.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Error("render lab fragment", "name", name, "err", err)
	}
}

func (a *App) labPage(w http.ResponseWriter, r *http.Request) {
	p, ok := personaByID(r.URL.Query().Get("as"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		if err := labChooserTmpl.Execute(w, personas); err != nil {
			a.logger.Error("render lab chooser", "err", err)
		}
		return
	}
	var other *persona
	for _, q := range personas {
		if q != p {
			other = q
		}
	}
	err := labPageTmpl.Execute(w, map[string]any{"P": p, "Other": other, "Conn": newConnID()})
	if err != nil {
		a.logger.Error("render lab page", "err", err)
	}
}

var (
	labPageTmpl    = template.Must(template.New("lab").Parse(labPageHTML))
	labChooserTmpl = template.Must(template.New("lab-chooser").Parse(labChooserHTML))
)
