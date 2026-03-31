package tavern

import (
	"fmt"
	"strings"
)

// SSEMessage represents a complete SSE message conforming to the SSE specification.
type SSEMessage struct {
	Event string
	Data  string
	ID    string
	Retry int
}

// String formats the SSEMessage as a wire-format SSE event.
func (m SSEMessage) String() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("event: %s", m.Event))
	parts = append(parts, fmt.Sprintf("data: %s", m.Data))
	if m.ID != "" {
		parts = append(parts, fmt.Sprintf("id: %s", m.ID))
	}
	if m.Retry > 0 {
		parts = append(parts, fmt.Sprintf("retry: %d", m.Retry))
	}
	return strings.Join(parts, "\n") + "\n\n"
}

// NewSSEMessage creates a new SSEMessage with the required event and data fields.
func NewSSEMessage(event, data string) SSEMessage {
	return SSEMessage{Event: event, Data: data}
}

// WithID adds an ID to the SSE message.
func (m SSEMessage) WithID(id string) SSEMessage {
	m.ID = id
	return m
}

// WithRetry adds a reconnection time (ms) to the SSE message.
func (m SSEMessage) WithRetry(ms int) SSEMessage {
	m.Retry = ms
	return m
}
