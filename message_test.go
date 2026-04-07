package tavern

import (
	"strings"
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

func TestSSEMessage_MultilineData(t *testing.T) {
	m := NewSSEMessage("update", "line1\nline2\nline3")
	got := m.String()

	assert.Contains(t, got, "data: line1\n")
	assert.Contains(t, got, "data: line2\n")
	assert.Contains(t, got, "data: line3\n")

	// There must be exactly 3 data: lines, not one collapsed line.
	count := strings.Count(got, "data: ")
	assert.Equal(t, 3, count, "each newline in Data must produce a separate data: line")
}

func TestSSEMessage_SingleLineData(t *testing.T) {
	m := NewSSEMessage("update", "hello")
	got := m.String()

	// Single line: exactly one data: field.
	count := strings.Count(got, "data: ")
	assert.Equal(t, 1, count)
	assert.Contains(t, got, "data: hello\n")
}

func TestIsControlEvent(t *testing.T) {
	assert.True(t, isControlEvent("event: tavern-reconnected\ndata: \n\n"))
	assert.True(t, isControlEvent("event: tavern-replay-gap\ndata: old-id\n\n"))
	assert.False(t, isControlEvent("event: metrics\ndata: cpu=42\n\n"))
	assert.False(t, isControlEvent("just raw text"))
	assert.False(t, isControlEvent(""))
}

func TestExtractSSEID(t *testing.T) {
	t.Run("no id", func(t *testing.T) {
		msg := "hello"
		cleaned, id := extractSSEID(msg)
		assert.Equal(t, msg, cleaned)
		assert.Empty(t, id)
	})

	t.Run("with id", func(t *testing.T) {
		msg := "id: 42\nhello"
		cleaned, id := extractSSEID(msg)
		assert.Equal(t, "42", id)
		assert.NotContains(t, cleaned, "id:")
		assert.Contains(t, cleaned, "hello")
	})

	t.Run("id in SSE frame", func(t *testing.T) {
		msg := "event: update\ndata: payload\nid: evt-7\n\n"
		cleaned, id := extractSSEID(msg)
		assert.Equal(t, "evt-7", id)
		assert.NotContains(t, cleaned, "id:")
		assert.Contains(t, cleaned, "event: update")
		assert.Contains(t, cleaned, "data: payload")
	})
}

func TestWrapForGroup(t *testing.T) {
	t.Run("control event passed through", func(t *testing.T) {
		ctrl := "event: tavern-reconnected\ndata: \n\n"
		got := wrapForGroup("metrics", ctrl)
		assert.Equal(t, ctrl, got, "control events should pass through unchanged")
	})

	t.Run("payload wrapped with topic", func(t *testing.T) {
		got := wrapForGroup("metrics", "cpu=42")
		assert.Contains(t, got, "event: metrics\n")
		assert.Contains(t, got, "data: cpu=42\n")
	})

	t.Run("payload with id preserved", func(t *testing.T) {
		got := wrapForGroup("alerts", "id: 7\ndisk-full")
		assert.Contains(t, got, "event: alerts\n")
		assert.Contains(t, got, "data: disk-full\n")
		assert.Contains(t, got, "id: 7\n")
	})

	t.Run("gap control event passed through", func(t *testing.T) {
		ctrl := replayGapControlEvent("old-id")
		got := wrapForGroup("t1", ctrl)
		assert.Equal(t, ctrl, got)
	})
}

func TestInjectSSEID_RawString(t *testing.T) {
	got := injectSSEID("hello", "42")
	assert.Contains(t, got, "hello")
	assert.Contains(t, got, "id: 42")
}

func TestInjectSSEID_EmptyID(t *testing.T) {
	msg := "hello"
	got := injectSSEID(msg, "")
	assert.Equal(t, msg, got)
}

func TestInjectSSEID_FormattedSSE(t *testing.T) {
	msg := "event: update\ndata: payload\n\n"
	got := injectSSEID(msg, "evt-1")
	assert.Contains(t, got, "event: update")
	assert.Contains(t, got, "data: payload")
	assert.Contains(t, got, "id: evt-1")
	assert.True(t, strings.HasSuffix(got, "\n\n"), "must preserve SSE frame terminator")
}

func TestInjectSSEID_ReplacesExistingID(t *testing.T) {
	msg := "event: update\ndata: payload\nid: old-id\n\n"
	got := injectSSEID(msg, "new-id")
	assert.Contains(t, got, "id: new-id")
	assert.NotContains(t, got, "old-id")
	// Only one id: field.
	assert.Equal(t, 1, strings.Count(got, "id:"))
}

func TestInjectSSEID_MultilineData(t *testing.T) {
	msg := "event: update\ndata: line1\ndata: line2\n\n"
	got := injectSSEID(msg, "ml-1")
	assert.Contains(t, got, "data: line1")
	assert.Contains(t, got, "data: line2")
	assert.Contains(t, got, "id: ml-1")
	assert.True(t, strings.HasSuffix(got, "\n\n"))
}
