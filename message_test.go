package tavern

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewSSEMessage(t *testing.T) {
	m := NewSSEMessage("update", "hello world")
	assert.Equal(t, "update", m.Event)
	assert.Equal(t, "hello world", m.Data)
	assert.Empty(t, m.ID)
	assert.Zero(t, m.Retry)
}

func TestSSEMessage_WithID(t *testing.T) {
	m := NewSSEMessage("update", "payload").WithID("evt-42")
	assert.Equal(t, "evt-42", m.ID)
	// Original fields untouched.
	assert.Equal(t, "update", m.Event)
	assert.Equal(t, "payload", m.Data)
}

func TestSSEMessage_WithRetry(t *testing.T) {
	m := NewSSEMessage("update", "payload").WithRetry(3000)
	assert.Equal(t, 3000, m.Retry)
	// Original fields untouched.
	assert.Equal(t, "update", m.Event)
	assert.Equal(t, "payload", m.Data)
}

func TestSSEMessage_WithID_WithRetry_Chained(t *testing.T) {
	m := NewSSEMessage("ping", "pong").WithID("id-1").WithRetry(1500)
	assert.Equal(t, "ping", m.Event)
	assert.Equal(t, "pong", m.Data)
	assert.Equal(t, "id-1", m.ID)
	assert.Equal(t, 1500, m.Retry)
}

func TestSSEMessage_String_BasicFields(t *testing.T) {
	m := NewSSEMessage("update", "hello")
	got := m.String()
	assert.Contains(t, got, "event: update\n")
	assert.Contains(t, got, "data: hello\n")
	// No id or retry fields.
	assert.NotContains(t, got, "id:")
	assert.NotContains(t, got, "retry:")
	// Must end with double newline.
	assert.True(t, len(got) >= 2 && got[len(got)-2:] == "\n\n")
}

func TestSSEMessage_String_WithID(t *testing.T) {
	m := NewSSEMessage("update", "payload").WithID("42")
	got := m.String()
	assert.Contains(t, got, "id: 42\n")
}

func TestSSEMessage_String_WithRetry(t *testing.T) {
	m := NewSSEMessage("update", "payload").WithRetry(5000)
	got := m.String()
	assert.Contains(t, got, "retry: 5000\n")
}

func TestSSEMessage_String_AllFields(t *testing.T) {
	m := NewSSEMessage("chat", "hi there").WithID("msg-7").WithRetry(2000)
	got := m.String()
	assert.Contains(t, got, "event: chat\n")
	assert.Contains(t, got, "data: hi there\n")
	assert.Contains(t, got, "id: msg-7\n")
	assert.Contains(t, got, "retry: 2000\n")
	assert.True(t, got[len(got)-2:] == "\n\n")
}

func TestSSEMessage_String_ZeroRetryOmitted(t *testing.T) {
	m := NewSSEMessage("update", "data").WithRetry(0)
	got := m.String()
	assert.NotContains(t, got, "retry:")
}

func TestSSEMessage_String_EmptyIDOmitted(t *testing.T) {
	m := NewSSEMessage("update", "data").WithID("")
	got := m.String()
	assert.NotContains(t, got, "id:")
}

func TestSSEMessage_WithID_ImmutableOriginal(t *testing.T) {
	orig := NewSSEMessage("event", "data")
	_ = orig.WithID("new-id")
	assert.Empty(t, orig.ID, "WithID should return a copy, not mutate the receiver")
}

func TestSSEMessage_WithRetry_ImmutableOriginal(t *testing.T) {
	orig := NewSSEMessage("event", "data")
	_ = orig.WithRetry(999)
	assert.Zero(t, orig.Retry, "WithRetry should return a copy, not mutate the receiver")
}
