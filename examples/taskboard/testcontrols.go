package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	// testHoldMax bounds a hold a test sets and never releases.
	testHoldMax = 30 * time.Second
	// testDrainMax bounds the wait for a window's queue to empty.
	testDrainMax = 5 * time.Second
)

var errControlsClosed = errors.New("the app is shutting down")

// testHold is a pauser a test controls. It releases itself after its
// limit, and once closed it can no longer be set.
type testHold struct {
	pauser
	limit time.Duration

	ctlMu  sync.Mutex
	timer  *time.Timer
	closed bool
}

func newTestHold(limit time.Duration) *testHold {
	return &testHold{pauser: pauser{resume: make(chan struct{})}, limit: limit}
}

func (h *testHold) set(on bool) error {
	h.ctlMu.Lock()
	defer h.ctlMu.Unlock()
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	if on && h.closed {
		return errControlsClosed
	}
	h.pauser.set(on)
	if on {
		var t *time.Timer
		t = time.AfterFunc(h.limit, func() {
			h.ctlMu.Lock()
			defer h.ctlMu.Unlock()
			if h.timer == t {
				h.timer = nil
				h.pauser.set(false)
			}
		})
		h.timer = t
	}
	return nil
}

func (h *testHold) held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

// close releases the hold for good.
func (h *testHold) close() {
	h.ctlMu.Lock()
	defer h.ctlMu.Unlock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.pauser.set(false)
}

// registerTestControls adds the endpoints the browser tests use to order
// events that a browser cannot order itself. They exist only with
// Config.TestControls:
//
//   - /test/lab/hold holds back the writes of one persona's open lab
//     window (an EventSource cannot be intercepted). The hold belongs to
//     each of that window's streams and ends with it.
//   - /test/lab/burst keeps a lab burst running after its last vote.
//   - /test/pressure/repair holds the pressure page's repair; releasing it
//     with drained=<conn> first waits for that window's queues to empty.
//
// Every hold releases itself after testHoldMax and at shutdown, and none
// can be set after shutdown.
func (a *App) registerTestControls() {
	if !a.cfg.TestControls {
		return
	}
	a.lab.testControls = true
	a.lab.holdBurst = newTestHold(testHoldMax)
	a.delivery.pressure.holdRepair = newTestHold(testHoldMax)
	a.delivery.pressure.queues = map[string]*prQueue{}

	a.mux.HandleFunc("POST /test/lab/hold", a.withPersona(func(w http.ResponseWriter, r *http.Request, p *persona) {
		controlResult(w, a.lab.holdWindow(p, r.URL.Query().Get("conn"), r.URL.Query().Get("on") == "1"))
	}))
	a.mux.HandleFunc("POST /test/lab/burst", func(w http.ResponseWriter, r *http.Request) {
		controlResult(w, a.lab.holdBurst.set(r.URL.Query().Get("hold") == "1"))
	})
	a.mux.HandleFunc("POST /test/pressure/repair", func(w http.ResponseWriter, r *http.Request) {
		l := a.delivery.pressure
		q := r.URL.Query()
		if q.Get("hold") == "1" {
			controlResult(w, l.holdRepair.set(true))
			return
		}
		var err error
		if conn := q.Get("drained"); conn != "" {
			ctx, cancel := context.WithTimeout(r.Context(), testDrainMax)
			err = l.awaitDrained(ctx, conn)
			cancel()
		}
		_ = l.holdRepair.set(false) // released whether or not the window drained
		controlResult(w, err)
	})
}

func (a *App) closeTestControls() {
	if !a.cfg.TestControls {
		return
	}
	a.lab.holdBurst.close()
	a.delivery.pressure.holdRepair.close()
}

func controlResult(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errNotFound):
		http.Error(w, "No such open window", http.StatusNotFound)
	case errors.Is(err, errControlsClosed):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		http.Error(w, "The window did not drain", http.StatusGatewayTimeout)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
