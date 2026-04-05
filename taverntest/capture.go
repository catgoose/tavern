package taverntest

import (
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
)

// Capture subscribes to a real broker and collects messages for assertion.
// Unlike [Recorder], Capture provides assertion methods that accept expected
// messages directly, making tests more declarative.
type Capture struct {
	msgs  []string
	mu    sync.Mutex
	unsub func()
	done  chan struct{}
}

// NewCapture subscribes to the given topic on the broker and starts
// collecting messages in a background goroutine. Call [Capture.Close]
// when done to unsubscribe and stop collection.
func NewCapture(broker *tavern.SSEBroker, topic string) *Capture {
	ch, unsub := broker.Subscribe(topic)
	return newCapture(ch, unsub)
}

// NewScopedCapture subscribes to the given topic and scope on the broker
// and starts collecting messages. Call [Capture.Close] when done.
func NewScopedCapture(broker *tavern.SSEBroker, topic, scope string) *Capture {
	ch, unsub := broker.SubscribeScoped(topic, scope)
	return newCapture(ch, unsub)
}

func newCapture(ch <-chan string, unsub func()) *Capture {
	c := &Capture{
		unsub: unsub,
		done:  make(chan struct{}),
	}
	go func() {
		defer close(c.done)
		for msg := range ch {
			c.mu.Lock()
			c.msgs = append(c.msgs, msg)
			c.mu.Unlock()
		}
	}()
	return c
}

// Close stops collecting and unsubscribes. Safe to call multiple times.
func (c *Capture) Close() {
	c.unsub()
	<-c.done
}

// Messages returns a copy of all collected messages so far.
func (c *Capture) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]string, len(c.msgs))
	copy(cp, c.msgs)
	return cp
}

// waitFor blocks until at least n messages are collected or timeout elapses.
func (c *Capture) waitFor(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		count := len(c.msgs)
		c.mu.Unlock()
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

// AssertReceived fails the test if the collected messages do not contain all
// of the expected messages (in any order). It waits up to 1 second for
// messages to arrive.
func (c *Capture) AssertReceived(t testing.TB, expected ...string) {
	t.Helper()
	if !c.waitFor(len(expected), time.Second) {
		c.mu.Lock()
		got := len(c.msgs)
		c.mu.Unlock()
		t.Fatalf("timed out waiting for %d messages, got %d", len(expected), got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, exp := range expected {
		found := false
		for _, msg := range c.msgs {
			if msg == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected message %q not found in %d collected messages", exp, len(c.msgs))
		}
	}
}

// AssertReceivedWithin is like [Capture.AssertReceived] but with a custom timeout.
func (c *Capture) AssertReceivedWithin(t testing.TB, timeout time.Duration, expected ...string) {
	t.Helper()
	if !c.waitFor(len(expected), timeout) {
		c.mu.Lock()
		got := len(c.msgs)
		c.mu.Unlock()
		t.Fatalf("timed out after %v waiting for %d messages, got %d", timeout, len(expected), got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, exp := range expected {
		found := false
		for _, msg := range c.msgs {
			if msg == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected message %q not found in %d collected messages", exp, len(c.msgs))
		}
	}
}

// AssertReceivedExactly fails the test if the collected messages do not match
// the expected messages exactly (same order, same count). It waits up to 1
// second for messages to arrive.
func (c *Capture) AssertReceivedExactly(t testing.TB, expected ...string) {
	t.Helper()
	if !c.waitFor(len(expected), time.Second) {
		c.mu.Lock()
		got := len(c.msgs)
		c.mu.Unlock()
		t.Fatalf("timed out waiting for %d messages, got %d", len(expected), got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) != len(expected) {
		t.Fatalf("expected %d messages, got %d", len(expected), len(c.msgs))
	}
	for i, exp := range expected {
		if c.msgs[i] != exp {
			t.Errorf("message[%d]: expected %q, got %q", i, exp, c.msgs[i])
		}
	}
}
