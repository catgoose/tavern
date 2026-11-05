package main

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/catgoose/tavern"
)

// The client page exercises tavern-js's lifecycle calls on regions the page
// adds and removes itself: Tavern.init, bind, scanAndBind and destroy, the
// stream registry, the updated class and opt-in debug logging. Every region
// is its own stream on one topic of a broker that belongs to this page, so
// the server's subscription count shows when htmx has closed a region's
// connection.
const (
	clTopic    = "client/pulse"
	clMaxOpen  = 8 // regions a window may have open
	clUpdateMs = 400
)

// pubMu orders each pulse's change and publish, and each new stream's
// snapshot and subscription, so a stream starts from the value its first
// live message follows and every stream sees the pulses in order.
type clientLab struct {
	broker *tavern.SSEBroker
	reg    connRegistry
	pubMu  sync.Mutex
	seq    int64

	opening func() // tests: called between a stream's snapshot and subscription
}

func newClientLab() *clientLab {
	return &clientLab{broker: tavern.NewSSEBroker(tavern.WithBufferSize(8))}
}

func (l *clientLab) pulse() int64 {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	l.seq++
	l.broker.Publish(clTopic, tavern.NewSSEMessage("pulse", fmt.Sprintf("pulse %d", l.seq)).String())
	return l.seq
}

func (l *clientLab) pulses() int64 {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	return l.seq
}

// open subscribes conn and returns the current value, to be written first.
func (l *clientLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), snapshot string, err error) {
	l.pubMu.Lock()
	defer l.pubMu.Unlock()
	snap := tavern.NewSSEMessage("pulse", fmt.Sprintf("pulse %d", l.seq)).String()
	if l.opening != nil {
		l.opening()
	}
	msgs, unsubscribe, err = openAll(func() (keyedSub, error) {
		ch, unsub := l.broker.SubscribeWith(clTopic, tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn}))
		return subOrErr(clTopic, ch, unsub)
	})
	return msgs, unsubscribe, snap, err
}

func (a *App) registerClient() {
	l := newClientLab()
	a.client = l
	a.mux.HandleFunc("GET /showcase/client", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := map[string]any{"Conn": newConnID(), "Debug": r.URL.Query().Get("debug") == "1", "UpdateMs": clUpdateMs, "Max": clMaxOpen}
		if err := clientPage.Execute(w, data); err != nil {
			a.logger.Error("render client page", "err", err)
		}
	})
	a.mux.HandleFunc("GET /showcase/client/events", func(w http.ResponseWriter, r *http.Request) {
		// The current value goes first: the response, and so the browser's
		// connection, opens with it.
		a.serveOwned(w, r, ownedStream{reg: &l.reg, openSnapshot: l.open})
	})
	a.mux.HandleFunc("POST /showcase/client/pulse", func(w http.ResponseWriter, _ *http.Request) {
		l.pulse()
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("GET /showcase/client/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"subscriptions": l.broker.SubscriberCount(), "pulses": l.pulses()})
	})
}
