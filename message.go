package tavern

import (
	"fmt"
	"strings"
)

// SSEMessage represents a complete Server-Sent Event message conforming to the
// W3C SSE specification (https://html.spec.whatwg.org/multipage/server-sent-events.html).
// Use [NewSSEMessage] to create one with the required fields, then chain
// [SSEMessage.WithID] or [SSEMessage.WithRetry] for optional fields.
type SSEMessage struct {
	// Event is the SSE event type (the "event:" field).
	Event string
	// Data is the event payload (the "data:" field).
	Data string
	// ID is the optional event identifier (the "id:" field). When set, the
	// browser will send it back as Last-Event-ID on reconnection.
	ID string
	// Retry is the optional reconnection time in milliseconds (the "retry:"
	// field). A zero value omits the field.
	Retry int
}

// String formats the SSEMessage as wire-format SSE text, terminated by a
// double newline as required by the specification.
func (m SSEMessage) String() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("event: %s", m.Event))
	for _, line := range strings.Split(m.Data, "\n") {
		parts = append(parts, "data: "+line)
	}
	if m.ID != "" {
		parts = append(parts, fmt.Sprintf("id: %s", m.ID))
	}
	if m.Retry > 0 {
		parts = append(parts, fmt.Sprintf("retry: %d", m.Retry))
	}
	return strings.Join(parts, "\n") + "\n\n"
}

// NewSSEMessage creates an [SSEMessage] with the required event type and data
// payload. Use the builder methods [SSEMessage.WithID] and
// [SSEMessage.WithRetry] to set optional fields.
func NewSSEMessage(event, data string) SSEMessage {
	return SSEMessage{Event: event, Data: data}
}

// WithID returns a copy of the message with the given event ID set. The
// browser uses this ID for reconnection via the Last-Event-ID header.
func (m SSEMessage) WithID(id string) SSEMessage {
	m.ID = id
	return m
}

// WithRetry returns a copy of the message with the reconnection time set to ms
// milliseconds. The browser will wait this long before attempting to reconnect
// after a connection loss.
func (m SSEMessage) WithRetry(ms int) SSEMessage {
	m.Retry = ms
	return m
}
