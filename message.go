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

// isControlEvent reports whether msg is a tavern control event (e.g.,
// tavern-reconnected, tavern-replay-gap). Control events are already
// fully-formatted SSE frames and must not be re-wrapped.
func isControlEvent(msg string) bool {
	return strings.HasPrefix(msg, "event: tavern-")
}

// extractSSEID extracts the id field value from an SSE message string and
// returns the message with the id field removed. If no id field is present, the
// original message is returned unchanged with an empty id.
func extractSSEID(msg string) (cleaned, id string) {
	lines := strings.Split(msg, "\n")
	var filtered []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "id:") {
			id = strings.TrimSpace(strings.TrimPrefix(trimmed, "id:"))
			continue
		}
		filtered = append(filtered, line)
	}
	if id == "" {
		return msg, ""
	}
	return strings.Join(filtered, "\n"), id
}

// wrapForGroup formats a raw subscriber message for grouped SSE output. Control
// events are forwarded as-is. Payload messages are wrapped with the topic as the
// SSE event type, preserving any id field injected by PublishWithID.
func wrapForGroup(topic, msg string) string {
	if isControlEvent(msg) {
		return msg
	}
	cleaned, id := extractSSEID(msg)
	m := NewSSEMessage(topic, cleaned)
	if id != "" {
		m = m.WithID(id)
	}
	return m.String()
}

// injectSSEID injects an "id: <id>" field into an SSE message string. If the
// message already contains an id: field, it is replaced. If the message is a
// properly terminated SSE frame (ending with \n\n), the id field is inserted
// before the final blank line. Otherwise it is prepended.
func injectSSEID(msg, id string) string {
	if id == "" {
		return msg
	}

	idLine := fmt.Sprintf("id: %s", id)

	// Remove any existing id: field to avoid duplicates.
	lines := strings.Split(msg, "\n")
	var filtered []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "id:") {
			continue
		}
		filtered = append(filtered, line)
	}

	// Rebuild the message. If it ended with \n\n (SSE frame terminator),
	// the split produces trailing empty strings. Insert id before those.
	// Find the last non-empty line index.
	lastNonEmpty := -1
	for i := len(filtered) - 1; i >= 0; i-- {
		if filtered[i] != "" {
			lastNonEmpty = i
			break
		}
	}

	if lastNonEmpty < 0 {
		// Message was empty or all blank lines; just return id field as SSE frame.
		return idLine + "\n\n"
	}

	// Insert id line after the last non-empty line, before trailing empty lines.
	var result []string
	result = append(result, filtered[:lastNonEmpty+1]...)
	result = append(result, idLine)
	result = append(result, filtered[lastNonEmpty+1:]...)
	return strings.Join(result, "\n")
}
