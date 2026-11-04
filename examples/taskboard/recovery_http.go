package main

import (
	"context"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/catgoose/tavern"
)

func (a *App) registerRecovery() {
	a.recovery = newRecovery()
	a.mux.HandleFunc("GET /showcase/recovery", a.recoveryIndex)
	a.mux.HandleFunc("GET /showcase/recovery/{name}", a.withScenario(a.recoveryPage))
	a.mux.HandleFunc("GET /showcase/recovery/{name}/events", a.withScenario(a.recoveryEvents))
	a.mux.HandleFunc("GET /showcase/recovery/{name}/state", a.withScenario(a.recoveryState))
	a.mux.HandleFunc("POST /showcase/recovery/{name}/change", a.withScenario(a.recoveryChange))
	a.mux.HandleFunc("POST /showcase/recovery/{name}/away", a.withScenario(a.recoveryAway))
	a.mux.HandleFunc("POST /showcase/recovery/{name}/pause", a.withScenario(a.recoveryPause))
	a.mux.HandleFunc("POST /showcase/recovery/{name}/clear", a.withScenario(func(w http.ResponseWriter, _ *http.Request, s *scenario) {
		s.clearReplay()
		w.WriteHeader(http.StatusNoContent)
	}))
	a.mux.HandleFunc("POST /showcase/recovery/{name}/fault", a.withScenario(a.recoveryFault))
}

func (a *App) withScenario(h func(http.ResponseWriter, *http.Request, *scenario)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.recovery.scenarios[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r, s)
	}
}

// count parses a bounded number of changes, at least lo.
func count(r *http.Request, def, lo int) (int, bool) {
	v := r.URL.Query().Get("n")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= lo && n <= recoveryAwayMax
}

// recoveryEvents streams one window's view of a scenario. While the window
// is away (see recoveryAway) its reconnect waits.
func (a *App) recoveryEvents(w http.ResponseWriter, r *http.Request, s *scenario) {
	conn := r.URL.Query().Get("conn")
	if conn == "" {
		http.Error(w, "Missing connection", http.StatusBadRequest)
		return
	}
	if h := s.holds.get(conn); h != nil {
		select {
		case <-h.released:
		case <-r.Context().Done():
			return
		case <-a.done:
			return
		}
	}
	lastID := r.Header.Get("Last-Event-ID")
	status := tavern.NewSubscriptionStatus()

	gate := &pauser{resume: make(chan struct{})}
	s.pausers.Store(conn, gate)
	defer func() {
		s.pausers.CompareAndDelete(conn, gate)
		gate.set(false)
	}()
	pw := pausableWriter{w, func() { gate.wait(r.Context(), a.done) }}
	opts := []tavern.StreamSSEOption{
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	}

	var err error
	if s.kind == kindMulti {
		ch, unsub := s.broker.SubscribeMultiFromID(s.topics, lastID)
		done := s.track(conn, unsub)
		defer done()
		err = tavern.StreamSSE(r.Context(), pw, ch, func(m tavern.TopicMessage) string { return m.Data }, opts...)
	} else {
		ch, unsub := s.subscribe(conn, lastID, status)
		if ch == nil {
			http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
			return
		}
		if s.kind == kindStoreStrict {
			ch, unsub = s.withFallback(r.Context(), conn, ch, unsub, status)
		}
		done := s.track(conn, unsub)
		defer done()
		err = tavern.StreamSSE(r.Context(), pw, ch, func(m string) string { return m }, opts...)
	}
	if reason := status.Termination().Reason; reason != tavern.TerminationNone && reason != tavern.TerminationUnsubscribe {
		s.observe(conn, "stream ended: %s", reason)
	}
	a.logger.Info("recovery stream ended", "scenario", s.name, "write_error", err)
}

// track registers conn's open stream so the away control can end it. The
// returned function unsubscribes and unregisters it.
func (s *scenario) track(conn string, unsub func()) func() {
	cs := &droppableStream{unsubscribe: unsub, done: make(chan struct{})}
	s.streams.Store(conn, cs)
	return func() {
		unsub()
		s.streams.CompareAndDelete(conn, cs)
		close(cs.done)
	}
}

// withFallback forwards ch and, if it ends because the replay store could
// not be read, continues with a snapshot subscription on the same response,
// so the window still reaches the current value.
func (s *scenario) withFallback(ctx context.Context, conn string, ch <-chan string, unsub func(), status *tavern.SubscriptionStatus) (msgs <-chan string, unsubscribe func()) {
	out := make(chan string)
	stop := make(chan struct{})
	finished := make(chan struct{})
	// forward copies in to out until in closes (true) or the stream stops.
	forward := func(in <-chan string) bool {
		for {
			select {
			case m, ok := <-in:
				if !ok {
					return true
				}
				select {
				case out <- m:
				case <-stop:
					return false
				case <-ctx.Done():
					return false
				}
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
		}
	}
	go func() {
		defer close(finished)
		defer close(out)
		if !forward(ch) || status.Termination().Reason != tavern.TerminationReplayFailure {
			return
		}
		s.observe(conn, "replay failed; continuing from a snapshot")
		next, fallbackUnsub := s.fallbackSubscribe()
		defer fallbackUnsub()
		forward(next)
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() { close(stop) })
		unsub()
		<-finished
	}
}

func (a *App) recoveryChange(w http.ResponseWriter, r *http.Request, s *scenario) {
	n, ok := count(r, 1, 1)
	if !ok {
		http.Error(w, "n must be from 1 to 20", http.StatusBadRequest)
		return
	}
	topic := s.topics[0]
	if t := r.URL.Query().Get("topic"); t != "" {
		if !slices.Contains(s.topics, t) {
			http.Error(w, "Unknown topic", http.StatusBadRequest)
			return
		}
		topic = t
	}
	for range n {
		s.change(topic)
	}
	w.WriteHeader(http.StatusNoContent)
}

// recoveryAway ends a window's stream, holds its reconnect, makes n changes,
// then lets it reconnect: an absence of exactly n changes. With n=0 it is a
// plain reconnect.
func (a *App) recoveryAway(w http.ResponseWriter, r *http.Request, s *scenario) {
	conn := r.URL.Query().Get("conn")
	n, ok := count(r, 3, 0)
	if !ok {
		http.Error(w, "n must be from 0 to 20", http.StatusBadRequest)
		return
	}
	v, open := s.streams.Load(conn)
	if !open {
		http.Error(w, "This window has no stream", http.StatusNotFound)
		return
	}
	if _, err := s.holds.add(conn); err != nil {
		http.Error(w, "Cannot disconnect now: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.holds.take(conn)
	cs := v.(*droppableStream)
	cs.unsubscribe()
	select {
	case <-cs.done:
	case <-r.Context().Done():
		return
	}
	for i := range n {
		s.change(s.topics[i%len(s.topics)])
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) recoveryPause(w http.ResponseWriter, r *http.Request, s *scenario) {
	v, ok := s.pausers.Load(r.URL.Query().Get("conn"))
	if !ok {
		http.Error(w, "This window has no stream", http.StatusNotFound)
		return
	}
	v.(*pauser).set(r.URL.Query().Get("on") == "1")
	w.WriteHeader(http.StatusNoContent)
}

// recoveryFault makes the shared replay store fail or work again.
func (a *App) recoveryFault(w http.ResponseWriter, r *http.Request, s *scenario) {
	if s.store == nil {
		http.Error(w, "This scenario has no replay store", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if v := q.Get("append"); v != "" {
		s.store.failAppend.Store(v == "1")
	}
	if v := q.Get("read"); v != "" {
		s.store.failRead.Store(v == "1")
	}
	w.WriteHeader(http.StatusNoContent)
}

type recoveryStateView struct {
	Values       map[string]int `json:"values"`
	Observations []observation  `json:"observations"`
	FailAppend   bool           `json:"failAppend"`
	FailRead     bool           `json:"failRead"`
}

func (a *App) recoveryState(w http.ResponseWriter, r *http.Request, s *scenario) {
	v := recoveryStateView{Values: s.current(), Observations: s.observations(r.URL.Query().Get("conn"))}
	if s.store != nil {
		v.FailAppend, v.FailRead = s.store.failAppend.Load(), s.store.failRead.Load()
	}
	writeJSON(w, v)
}

func (a *App) recoveryIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := recoveryIndexTmpl.Execute(w, a.recovery.order); err != nil {
		a.logger.Error("render recovery index", "err", err)
	}
}

// gapActions are the tavern-gap-action values the pages offer.
var gapActions = map[string]bool{"": true, "reload": true, "banner": true, "recovery-gap": true}

func (a *App) recoveryPage(w http.ResponseWriter, r *http.Request, s *scenario) {
	action := r.URL.Query().Get("action")
	if !gapActions[action] {
		http.Error(w, "Unknown gap action", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := recoveryPageTmpl.Execute(w, map[string]any{
		"S": s, "Conn": newConnID(), "Action": action,
		"Pressure": s.kind == kindPressure, "Store": s.store != nil, "Multi": s.kind == kindMulti,
	})
	if err != nil {
		a.logger.Error("render recovery page", "err", err)
	}
}

var (
	recoveryIndexTmpl = template.Must(template.New("recovery-index").Parse(recoveryIndexHTML))
	recoveryPageTmpl  = template.Must(template.New("recovery").Parse(recoveryPageHTML))
)
