package taverntest

import (
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
)

// Recorder captures messages published to a topic for test assertions.
// It subscribes to the topic on creation and collects messages in a
// background goroutine. Use [NewRecorder] or [NewScopedRecorder] to create
// one. Recorder is safe for concurrent use by multiple goroutines.
type Recorder struct {
	msgs   []string
	mu     sync.Mutex
	unsub  func()
	done   chan struct{}
	closed bool
}

// NewRecorder subscribes to the given topic on the broker and starts
// collecting messages in a background goroutine. Call [Recorder.Close]
// when done to unsubscribe and stop collection.
func NewRecorder(broker *tavern.SSEBroker, topic string) *Recorder {
	ch, unsub := broker.Subscribe(topic)
	return newRecorder(ch, unsub)
}

// NewScopedRecorder subscribes to the given topic and scope on the broker
// and starts collecting messages. Call [Recorder.Close] when done.
func NewScopedRecorder(broker *tavern.SSEBroker, topic, scope string) *Recorder {
	ch, unsub := broker.SubscribeScoped(topic, scope)
	return newRecorder(ch, unsub)
}

func newRecorder(ch <-chan string, unsub func()) *Recorder {
	r := &Recorder{
		unsub: unsub,
		done:  make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		for msg := range ch {
			r.mu.Lock()
			r.msgs = append(r.msgs, msg)
			r.mu.Unlock()
		}
	}()
	return r
}

// Close stops collecting messages and unsubscribes from the topic.
// Safe to call multiple times.
func (r *Recorder) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.unsub()
	<-r.done // wait for collection goroutine to finish
}

// Messages returns a copy of all collected messages so far.
func (r *Recorder) Messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.msgs))
	copy(cp, r.msgs)
	return cp
}

// Len returns the number of messages collected so far.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// WaitFor blocks until at least n messages have been collected or the
// timeout elapses. Returns true if n messages were collected, false on
// timeout.
func (r *Recorder) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		r.mu.Lock()
		count := len(r.msgs)
		r.mu.Unlock()
		if count >= n {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

// AssertCount fails the test if the number of collected messages does not
// equal expected.
func (r *Recorder) AssertCount(t testing.TB, expected int) {
	t.Helper()
	r.mu.Lock()
	got := len(r.msgs)
	r.mu.Unlock()
	if got != expected {
		t.Errorf("expected %d messages, got %d", expected, got)
	}
}

// AssertContains fails the test if none of the collected messages equal msg.
func (r *Recorder) AssertContains(t testing.TB, msg string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if m == msg {
			return
		}
	}
	t.Errorf("expected message %q not found in %d collected messages", msg, len(r.msgs))
}

// AssertNthMessage fails the test if the n-th collected message (0-indexed)
// does not equal expected.
func (r *Recorder) AssertNthMessage(t testing.TB, n int, expected string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.msgs) {
		t.Errorf("expected message at index %d, but only %d messages collected", n, len(r.msgs))
		return
	}
	if r.msgs[n] != expected {
		t.Errorf("message[%d]: expected %q, got %q", n, expected, r.msgs[n])
	}
}
