package main

import (
	"errors"
	"sync"
)

// maxOpenStreams bounds the open streams one registry owns.
const maxOpenStreams = 32

var (
	errConnInUse = errors.New("this connection already has an open stream")
	errConnsFull = errors.New("too many open streams")
)

// ownedConn owns one window's open stream: its pause gate and the means to
// end it.
type ownedConn struct {
	gate        *pauser
	done        chan struct{} // closed once the stream is torn down and released
	unsubscribe func()        // set under the registry's lock once subscribed
}

func newOwnedConn() *ownedConn {
	return &ownedConn{gate: &pauser{resume: make(chan struct{})}, done: make(chan struct{})}
}

// connRegistry gives each connection ID at most one owner. An ID is
// reserved before its gate or subscription exists, so a second request with
// the same ID cannot take over another window's controls.
type connRegistry struct {
	connMu sync.Mutex
	conns  map[string]*ownedConn
}

// reserve makes c the owner of conn, unless conn is already owned.
func (r *connRegistry) reserve(conn string, c *ownedConn) error {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if _, ok := r.conns[conn]; ok {
		return errConnInUse
	}
	if len(r.conns) >= maxOpenStreams {
		return errConnsFull
	}
	if r.conns == nil {
		r.conns = map[string]*ownedConn{}
	}
	r.conns[conn] = c
	return nil
}

// release ends c's ownership of conn; a later owner is left alone.
func (r *connRegistry) release(conn string, c *ownedConn) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if r.conns[conn] == c {
		delete(r.conns, conn)
	}
}

// owner is conn's current owner, if any.
func (r *connRegistry) owner(conn string) *ownedConn {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	return r.conns[conn]
}

func (r *connRegistry) setUnsubscribe(c *ownedConn, unsub func()) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	c.unsubscribe = unsub
}

// subscribed is conn's owner and its unsubscribe, once it has subscribed.
func (r *connRegistry) subscribed(conn string) (owner *ownedConn, unsubscribe func()) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	c := r.conns[conn]
	if c == nil || c.unsubscribe == nil {
		return nil, nil
	}
	return c, c.unsubscribe
}

func (r *connRegistry) openStreams() int {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	return len(r.conns)
}

// each calls fn for every current owner, under the registry's lock; fn
// must not call back into the registry.
func (r *connRegistry) each(fn func(conn string, c *ownedConn)) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	for conn, c := range r.conns {
		fn(conn, c)
	}
}
