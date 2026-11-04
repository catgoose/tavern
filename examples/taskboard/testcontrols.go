package main

import (
	"net/http"
	"sync"
)

// registerTestControls adds the endpoints the browser tests use to order
// events that a browser cannot order itself: holding back one window's lab
// stream (an EventSource cannot be intercepted), keeping a lab burst
// running, and holding the pressure page's repair until a window has
// drained. They exist only with Config.TestControls, and every hold is
// released when its stream ends or the app shuts down.
func (a *App) registerTestControls() {
	if !a.cfg.TestControls {
		return
	}
	a.lab.holdBurst = &pauser{resume: make(chan struct{})}
	a.lab.holds = &sync.Map{}
	a.mux.HandleFunc("POST /test/lab/hold", func(w http.ResponseWriter, r *http.Request) {
		conn := r.URL.Query().Get("conn")
		if conn == "" {
			http.Error(w, "Missing connection", http.StatusBadRequest)
			return
		}
		v, _ := a.lab.holds.LoadOrStore(conn, &pauser{resume: make(chan struct{})})
		on := r.URL.Query().Get("on") == "1"
		v.(*pauser).set(on)
		if !on {
			a.lab.holds.CompareAndDelete(conn, v)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	a.mux.HandleFunc("POST /test/lab/burst", func(w http.ResponseWriter, r *http.Request) {
		a.lab.holdBurst.set(r.URL.Query().Get("hold") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	a.delivery.pressure.holdRepair = &pauser{resume: make(chan struct{})}
	a.mux.HandleFunc("POST /test/pressure/repair", func(w http.ResponseWriter, r *http.Request) {
		a.delivery.pressure.holdRepair.set(r.URL.Query().Get("hold") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
}
