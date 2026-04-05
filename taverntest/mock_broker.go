package taverntest

import (
	"sync"
	"testing"

	"github.com/catgoose/tavern"
)

// publishedCall records a single Publish or PublishOOB call.
type publishedCall struct {
	Topic    string
	Msg      string
	Scope    string // non-empty for scoped publishes
	OOB      bool
	Fragments []tavern.Fragment
}

// MockBroker records publish calls for test assertions. It does not deliver
// messages to any subscribers; use it when you want to verify that application
// code publishes the right messages without running a real broker.
type MockBroker struct {
	mu   sync.Mutex
	calls []publishedCall
}

// NewMockBroker creates a ready-to-use MockBroker.
func NewMockBroker() *MockBroker {
	return &MockBroker{}
}

// Publish records a publish call for the given topic and message.
func (m *MockBroker) Publish(topic, msg string) {
	m.mu.Lock()
	m.calls = append(m.calls, publishedCall{Topic: topic, Msg: msg})
	m.mu.Unlock()
}

// PublishTo records a scoped publish call.
func (m *MockBroker) PublishTo(topic, scope, msg string) {
	m.mu.Lock()
	m.calls = append(m.calls, publishedCall{Topic: topic, Msg: msg, Scope: scope})
	m.mu.Unlock()
}

// PublishOOB records an OOB publish call. The fragments are rendered via
// [tavern.RenderFragments] so that AssertPublished can match on the rendered
// output, and the raw fragments are also stored for inspection.
func (m *MockBroker) PublishOOB(topic string, fragments ...tavern.Fragment) {
	rendered := tavern.RenderFragments(fragments...)
	m.mu.Lock()
	m.calls = append(m.calls, publishedCall{
		Topic:     topic,
		Msg:       rendered,
		OOB:       true,
		Fragments: fragments,
	})
	m.mu.Unlock()
}

// PublishOOBTo records a scoped OOB publish call.
func (m *MockBroker) PublishOOBTo(topic, scope string, fragments ...tavern.Fragment) {
	rendered := tavern.RenderFragments(fragments...)
	m.mu.Lock()
	m.calls = append(m.calls, publishedCall{
		Topic:     topic,
		Msg:       rendered,
		Scope:     scope,
		OOB:       true,
		Fragments: fragments,
	})
	m.mu.Unlock()
}

// Calls returns a copy of all recorded publish calls.
func (m *MockBroker) Calls() []publishedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]publishedCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

// Reset clears all recorded calls.
func (m *MockBroker) Reset() {
	m.mu.Lock()
	m.calls = nil
	m.mu.Unlock()
}

// callsForTopic returns all recorded calls matching topic.
func (m *MockBroker) callsForTopic(topic string) []publishedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []publishedCall
	for _, c := range m.calls {
		if c.Topic == topic {
			result = append(result, c)
		}
	}
	return result
}

// AssertPublished fails the test if no call matches the given topic and message.
func (m *MockBroker) AssertPublished(t testing.TB, topic, msg string) {
	t.Helper()
	calls := m.callsForTopic(topic)
	for _, c := range calls {
		if c.Msg == msg {
			return
		}
	}
	t.Errorf("expected publish(%q, %q) not found in %d calls for topic", topic, msg, len(calls))
}

// AssertPublishedCount fails the test if the number of calls for the given
// topic does not equal n.
func (m *MockBroker) AssertPublishedCount(t testing.TB, topic string, n int) {
	t.Helper()
	calls := m.callsForTopic(topic)
	if len(calls) != n {
		t.Errorf("expected %d publishes to %q, got %d", n, topic, len(calls))
	}
}

// AssertNotPublished fails the test if any call was recorded for the given topic.
func (m *MockBroker) AssertNotPublished(t testing.TB, topic string) {
	t.Helper()
	calls := m.callsForTopic(topic)
	if len(calls) > 0 {
		t.Errorf("expected no publishes to %q, got %d", topic, len(calls))
	}
}
