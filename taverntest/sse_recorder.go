package taverntest

import (
	"bufio"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// SSEEvent represents a parsed Server-Sent Event from the SSE stream.
type SSEEvent struct {
	// Event is the event type (from the "event:" field). Empty if not set.
	Event string
	// Data is the event payload (concatenated "data:" fields, joined by newlines).
	Data string
	// ID is the event identifier (from the "id:" field). Empty if not set.
	ID string
	// Retry is the raw retry value (from the "retry:" field). Empty if not set.
	Retry string
}

// SSERecorder is an [http.ResponseWriter] and [http.Flusher] that captures
// SSE-formatted output written by handlers. It parses the SSE wire format
// into structured [SSEEvent] values for easy assertion.
//
// Use it like [net/http/httptest.ResponseRecorder] but with SSE-aware
// assertion methods. SSERecorder is safe for concurrent use by multiple
// goroutines.
type SSERecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	code    int
	flushed int
}

// NewSSERecorder creates a ready-to-use SSERecorder.
func NewSSERecorder() *SSERecorder {
	return &SSERecorder{
		header: make(http.Header),
		code:   http.StatusOK,
	}
}

// Header returns the response headers.
func (r *SSERecorder) Header() http.Header {
	return r.header
}

// Write writes data to the response body.
func (r *SSERecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}

// WriteHeader records the HTTP status code.
func (r *SSERecorder) WriteHeader(code int) {
	r.mu.Lock()
	r.code = code
	r.mu.Unlock()
}

// Flush records that a flush occurred.
func (r *SSERecorder) Flush() {
	r.mu.Lock()
	r.flushed++
	r.mu.Unlock()
}

// Code returns the recorded HTTP status code.
func (r *SSERecorder) Code() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}

// Body returns the raw response body as a string.
func (r *SSERecorder) Body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// FlushCount returns how many times Flush was called.
func (r *SSERecorder) FlushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushed
}

// Events parses the recorded body as an SSE stream and returns the events.
// Each event is delimited by a blank line (double newline) per the SSE spec.
// Comment lines (starting with ":") are ignored.
func (r *SSERecorder) Events() []SSEEvent {
	r.mu.Lock()
	raw := r.body.String()
	r.mu.Unlock()
	return parseSSEStream(raw)
}

// parseSSEStream parses an SSE wire-format string into events.
func parseSSEStream(raw string) []SSEEvent {
	var events []SSEEvent
	scanner := bufio.NewScanner(strings.NewReader(raw))

	var current SSEEvent
	var dataLines []string
	hasFields := false

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line = event boundary
		if line == "" {
			if hasFields {
				current.Data = strings.Join(dataLines, "\n")
				events = append(events, current)
			}
			current = SSEEvent{}
			dataLines = nil
			hasFields = false
			continue
		}

		// Comment lines
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse field: value
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			current.Event = value
			hasFields = true
		case "data":
			dataLines = append(dataLines, value)
			hasFields = true
		case "id":
			current.ID = value
			hasFields = true
		case "retry":
			current.Retry = value
			hasFields = true
		}
	}

	// Handle trailing event without final blank line
	if hasFields {
		current.Data = strings.Join(dataLines, "\n")
		events = append(events, current)
	}

	return events
}

// AssertEvent fails the test if no recorded event matches the given event
// name and data.
func (r *SSERecorder) AssertEvent(t testing.TB, eventName, data string) {
	t.Helper()
	events := r.Events()
	for _, e := range events {
		if e.Event == eventName && e.Data == data {
			return
		}
	}
	t.Errorf("expected SSE event(event=%q, data=%q) not found in %d events", eventName, data, len(events))
}

// AssertEventCount fails the test if the number of recorded events does not
// equal n.
func (r *SSERecorder) AssertEventCount(t testing.TB, n int) {
	t.Helper()
	events := r.Events()
	if len(events) != n {
		t.Errorf("expected %d SSE events, got %d", n, len(events))
	}
}

// AssertHasID fails the test if no recorded event has the given ID.
func (r *SSERecorder) AssertHasID(t testing.TB, id string) {
	t.Helper()
	events := r.Events()
	for _, e := range events {
		if e.ID == id {
			return
		}
	}
	t.Errorf("expected SSE event with id=%q not found in %d events", id, len(events))
}

// AssertContentType fails the test if the Content-Type header is not
// "text/event-stream".
func (r *SSERecorder) AssertContentType(t testing.TB) {
	t.Helper()
	ct := r.header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected Content-Type %q, got %q", "text/event-stream", ct)
	}
}

// AssertFlushed fails the test if Flush was never called.
func (r *SSERecorder) AssertFlushed(t testing.TB) {
	t.Helper()
	if r.FlushCount() == 0 {
		t.Error("expected at least one Flush call")
	}
}
