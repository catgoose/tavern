package main

import (
	"html/template"
	"net/http"
	"strconv"
	"sync"

	"github.com/catgoose/tavern"
)

// topicCounter carries the connection scenario's counter. Each message is the
// whole current value with an event ID. A fresh connection replays the values
// still kept, ending with the current one, so it never misses a change made
// while the page loaded; a reconnect replays only what it missed.
const topicCounter = "showcase/counter"

// counterReplay is how many counter events the broker keeps. A window can
// miss up to counterReplay-1 changes and still resume without a gap.
const counterReplay = 4

// counterScenario is the showcase's connection and live update scenario.
type counterScenario struct {
	mu    sync.Mutex
	count int

	// gate holds new subscriptions back while a drop is in progress, so the
	// dropped window cannot reconnect before the value it should miss.
	gate    sync.RWMutex
	streams sync.Map // connection id → *counterStream, while its stream is open
}

// counterStream lets the demo end one window's stream.
type counterStream struct {
	unsubscribe func()
	done        chan struct{} // closed after the stream has ended
}

func (a *App) registerShowcase() {
	a.broker.SetReplayPolicy(topicCounter, counterReplay)
	// The starting value gives every new stream a first frame, which is when
	// the browser sees it open.
	a.counter.publish(a.broker)
	a.mux.HandleFunc("GET /showcase", a.showcaseIndex)
	a.mux.HandleFunc("GET /showcase/connection", a.connectionPage)
	a.mux.HandleFunc("GET /showcase/connection/events", a.counterEvents)
	a.mux.HandleFunc("POST /showcase/connection/increment", a.counterIncrement)
	a.mux.HandleFunc("POST /showcase/connection/drop", a.counterDrop)
}

// increment raises the counter and publishes the new value. The lock keeps
// event IDs and values in publish order.
func (s *counterScenario) increment(b *tavern.SSEBroker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	s.publishLocked(b)
}

func (s *counterScenario) publish(b *tavern.SSEBroker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(b)
}

func (s *counterScenario) publishLocked(b *tavern.SSEBroker) {
	v := strconv.Itoa(s.count)
	b.PublishWithID(topicCounter, v, tavern.NewSSEMessage("counter", v).String())
}

func (s *counterScenario) value() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// counterEvents streams the counter. A reconnect sends Last-Event-ID, and
// the broker replays the values after it, then sends tavern-reconnected.
func (a *App) counterEvents(w http.ResponseWriter, r *http.Request) {
	a.counter.gate.RLock()
	ch, unsub := a.broker.SubscribeFromIDWith(topicCounter, r.Header.Get("Last-Event-ID"))
	a.counter.gate.RUnlock()
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	conn := r.URL.Query().Get("conn")
	cs := &counterStream{unsubscribe: unsub, done: make(chan struct{})}
	a.counter.streams.Store(conn, cs)
	defer func() {
		unsub()
		a.counter.streams.CompareAndDelete(conn, cs)
		close(cs.done)
	}()

	err := tavern.StreamSSE(r.Context(), w, ch, func(msg string) string { return msg },
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	a.logger.Info("counter stream ended", "write_error", err)
}

func (a *App) counterIncrement(w http.ResponseWriter, _ *http.Request) {
	a.counter.increment(a.broker)
	w.WriteHeader(http.StatusNoContent)
}

// counterDrop ends this window's stream from the server, as a restart or a
// load balancer closing the connection would. With away=1 it also changes
// the counter after the stream has ended and before the window can
// reconnect, so the reconnect has a missed value to replay.
func (a *App) counterDrop(w http.ResponseWriter, r *http.Request) {
	v, ok := a.counter.streams.Load(r.URL.Query().Get("conn"))
	if !ok {
		http.Error(w, "This tab has no live connection", http.StatusNotFound)
		return
	}
	cs := v.(*counterStream)
	a.counter.gate.Lock()
	defer a.counter.gate.Unlock()
	cs.unsubscribe() // closes the channel, so the stream ends cleanly
	select {
	case <-cs.done:
	case <-r.Context().Done():
		return
	}
	if r.URL.Query().Get("away") == "1" {
		a.counter.increment(a.broker)
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	showcaseTmpl   = template.Must(template.New("showcase").Parse(showcaseHTML))
	connectionTmpl = template.Must(template.New("connection").Parse(connectionHTML))
)

func (a *App) showcaseIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := showcaseTmpl.Execute(w, nil); err != nil {
		a.logger.Error("render showcase", "err", err)
	}
}

func (a *App) connectionPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := connectionTmpl.Execute(w, map[string]any{"Conn": newConnID(), "Count": a.counter.value()})
	if err != nil {
		a.logger.Error("render connection scenario", "err", err)
	}
}
