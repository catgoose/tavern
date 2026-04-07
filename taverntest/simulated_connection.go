package taverntest

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
)

// SimulatedConnection models a client that can disconnect and reconnect to a
// broker, collecting messages across connection cycles. This is useful for
// testing Last-Event-ID resumption, reconnect behavior, and message
// continuity across disconnects. SimulatedConnection is safe for concurrent
// use by multiple goroutines.
type SimulatedConnection struct {
	broker *tavern.SSEBroker
	topic  string

	mu              sync.Mutex
	msgs            []string
	reconnectMsgs   []string // messages received after the most recent reconnect
	unsub           func()
	done            chan struct{}
	connected       bool
}

// NewSimulatedConnection subscribes to the given topic and starts collecting
// messages. Call [SimulatedConnection.Close] when done.
func NewSimulatedConnection(broker *tavern.SSEBroker, topic string) *SimulatedConnection {
	sc := &SimulatedConnection{
		broker: broker,
		topic:  topic,
	}
	sc.connect("")
	return sc
}

func (sc *SimulatedConnection) connect(lastEventID string) {
	var ch <-chan string
	var unsub func()
	if lastEventID != "" {
		ch, unsub = sc.broker.SubscribeFromID(sc.topic, lastEventID)
	} else {
		ch, unsub = sc.broker.Subscribe(sc.topic)
	}
	done := make(chan struct{})
	sc.mu.Lock()
	sc.unsub = unsub
	sc.done = done
	sc.connected = true
	sc.reconnectMsgs = nil
	sc.mu.Unlock()

	go func() {
		defer close(done)
		for msg := range ch {
			sc.mu.Lock()
			sc.msgs = append(sc.msgs, msg)
			sc.reconnectMsgs = append(sc.reconnectMsgs, msg)
			sc.mu.Unlock()
		}
	}()
}

// Disconnect unsubscribes from the broker, simulating a client disconnect.
func (sc *SimulatedConnection) Disconnect() {
	sc.mu.Lock()
	if !sc.connected {
		sc.mu.Unlock()
		return
	}
	sc.connected = false
	unsub := sc.unsub
	done := sc.done
	sc.mu.Unlock()
	unsub()
	<-done
}

// Reconnect re-subscribes to the topic with the given lastEventID, simulating
// a client reconnect with Last-Event-ID resumption. Pass an empty string to
// reconnect without resumption.
func (sc *SimulatedConnection) Reconnect(lastEventID string) {
	sc.Disconnect()
	sc.connect(lastEventID)
}

// Close disconnects and releases resources. Safe to call multiple times.
func (sc *SimulatedConnection) Close() {
	sc.Disconnect()
}

// Messages returns a copy of all messages received across all connection
// cycles.
func (sc *SimulatedConnection) Messages() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	cp := make([]string, len(sc.msgs))
	copy(cp, sc.msgs)
	return cp
}

// ReconnectMessages returns a copy of messages received since the most
// recent connect or reconnect.
func (sc *SimulatedConnection) ReconnectMessages() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	cp := make([]string, len(sc.reconnectMsgs))
	copy(cp, sc.reconnectMsgs)
	return cp
}

// Connected reports whether the simulated connection is currently active.
func (sc *SimulatedConnection) Connected() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.connected
}

// WaitFor blocks until at least n total messages have been received or the
// timeout elapses. Returns true if n messages were collected, false on timeout.
func (sc *SimulatedConnection) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		sc.mu.Lock()
		count := len(sc.msgs)
		sc.mu.Unlock()
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

// WaitForReconnect blocks until at least n messages have been received since
// the most recent reconnect, or the timeout elapses.
func (sc *SimulatedConnection) WaitForReconnect(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		sc.mu.Lock()
		count := len(sc.reconnectMsgs)
		sc.mu.Unlock()
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

// AssertReceivedAfterReconnect fails the test if the messages received since
// the most recent reconnect do not contain all of the expected messages (in
// any order). It waits up to 1 second for messages to arrive.
func (sc *SimulatedConnection) AssertReceivedAfterReconnect(t testing.TB, expected ...string) {
	t.Helper()
	if !sc.WaitForReconnect(len(expected), time.Second) {
		sc.mu.Lock()
		got := len(sc.reconnectMsgs)
		sc.mu.Unlock()
		t.Fatalf("timed out waiting for %d reconnect messages, got %d", len(expected), got)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, exp := range expected {
		found := false
		for _, msg := range sc.reconnectMsgs {
			if msg == exp || strings.Contains(msg, exp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected message %q not found in reconnect messages: %v", exp, sc.reconnectMsgs)
		}
	}
}
