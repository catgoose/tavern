// Package memory provides an in-process Backend implementation for testing
// and single-instance deployments. Multiple linked backends can be created
// via Fork to simulate cross-process fan-out within a single process.
package memory

import (
	"context"
	"sync"

	"github.com/catgoose/tavern/backend"
)

// bus is the shared message transport that links forked backends together.
type bus struct {
	mu   sync.RWMutex
	subs map[string]map[*Backend]chan backend.MessageEnvelope // topic → backend → channel
}

func newBus() *bus {
	return &bus{
		subs: make(map[string]map[*Backend]chan backend.MessageEnvelope),
	}
}

// publish fans out env to all backends subscribed to env.Topic except the
// sender.
func (b *bus) publish(sender *Backend, env backend.MessageEnvelope) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for be, ch := range b.subs[env.Topic] {
		if be == sender {
			continue
		}
		select {
		case ch <- env:
		default:
			// drop if the receiver is slow — same as the broker's local fan-out
		}
	}
}

// subscribe registers a backend for the given topic and returns a channel.
func (b *bus) subscribe(be *Backend, topic string) chan backend.MessageEnvelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan backend.MessageEnvelope, 64)
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[*Backend]chan backend.MessageEnvelope)
	}
	b.subs[topic][be] = ch
	return ch
}

// unsubscribe removes a backend from the given topic and closes its channel.
func (b *bus) unsubscribe(be *Backend, topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[topic][be]; ok {
		close(ch)
		delete(b.subs[topic], be)
	}
}

// removeAll removes all subscriptions for the given backend and closes their
// channels.
func (b *bus) removeAll(be *Backend) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for topic, backends := range b.subs {
		if ch, ok := backends[be]; ok {
			close(ch)
			delete(backends, be)
		}
		if len(backends) == 0 {
			delete(b.subs, topic)
		}
	}
}

// Backend is an in-process implementation of [backend.Backend] backed by a
// shared bus. Use [New] to create one. Call [Backend.Fork] to create a linked
// instance that shares the same bus — publishes on either instance are visible
// to subscribers on the other.
type Backend struct {
	bus    *bus
	mu     sync.Mutex
	subs   map[string]chan backend.MessageEnvelope
	closed bool
}

// New creates a new in-process Backend with its own bus.
func New() *Backend {
	return &Backend{
		bus:  newBus(),
		subs: make(map[string]chan backend.MessageEnvelope),
	}
}

// Fork creates a new Backend that shares the same message bus as the
// receiver. Messages published on any fork are delivered to subscribers on
// all other forks (but not back to the publisher).
func (b *Backend) Fork() *Backend {
	return &Backend{
		bus:  b.bus,
		subs: make(map[string]chan backend.MessageEnvelope),
	}
}

// Publish sends env to all other backends subscribed to the same topic.
func (b *Backend) Publish(_ context.Context, env backend.MessageEnvelope) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	b.mu.Unlock()
	b.bus.publish(b, env)
	return nil
}

// Subscribe registers interest in a topic and returns a channel that receives
// envelopes published by other backends sharing the same bus.
func (b *Backend) Subscribe(_ context.Context, topic string) (<-chan backend.MessageEnvelope, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if _, ok := b.subs[topic]; ok {
		// Already subscribed — return existing channel.
		return b.subs[topic], nil
	}
	ch := b.bus.subscribe(b, topic)
	b.subs[topic] = ch
	return ch, nil
}

// Unsubscribe removes the subscription for the given topic.
func (b *Backend) Unsubscribe(topic string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if _, ok := b.subs[topic]; !ok {
		return nil
	}
	delete(b.subs, topic)
	b.bus.unsubscribe(b, topic)
	return nil
}

// Close shuts down the backend and closes all subscription channels.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.bus.removeAll(b)
	b.subs = nil
	return nil
}

// ErrClosed is returned when an operation is attempted on a closed backend.
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "memory backend: closed" }
