package tavern

import (
	"strings"
	"testing"
)

func FuzzSSEMessageString(f *testing.F) {
	// Seed corpus: normal cases and edge cases.
	f.Add("update", "hello world", "", 0)
	f.Add("message", "line1\nline2\nline3", "evt-1", 3000)
	f.Add("", "", "", 0)
	f.Add("event:with:colons", "data: with prefix", "id: with space", 100)
	f.Add("update", "\n\n\n", "abc", 1)
	f.Add("a/b/c", "foo\nbar", "", -1)
	f.Add("tavern-reconnected", "control", "", 0)
	f.Add("event\nnewline", "data\nnewline", "id\nnewline", 0)
	f.Add("event\x00null", "data\x00null", "id\x00null", 0)
	f.Add(strings.Repeat("x", 10000), strings.Repeat("y", 10000), strings.Repeat("z", 10000), 999999)

	f.Fuzz(func(t *testing.T, event, data, id string, retry int) {
		m := NewSSEMessage(event, data)
		if id != "" {
			m = m.WithID(id)
		}
		if retry > 0 {
			m = m.WithRetry(retry)
		}

		s := m.String()

		// Invariant: SSE messages must end with double newline.
		if !strings.HasSuffix(s, "\n\n") {
			t.Errorf("SSE message does not end with \\n\\n: %q", s)
		}

		// Invariant: must contain an event: line.
		if !strings.Contains(s, "event: ") {
			t.Errorf("SSE message missing event: field: %q", s)
		}

		// Invariant: must contain at least one data: line.
		if !strings.Contains(s, "data: ") {
			t.Errorf("SSE message missing data: field: %q", s)
		}

		// Invariant: if ID was set, must contain id: line.
		if id != "" && !strings.Contains(s, "id: ") {
			t.Errorf("SSE message with ID %q missing id: field: %q", id, s)
		}

		// Invariant: if retry was set and positive, must contain retry: line.
		if retry > 0 && !strings.Contains(s, "retry: ") {
			t.Errorf("SSE message with retry %d missing retry: field: %q", retry, s)
		}
	})
}

func FuzzInjectSSEID(f *testing.F) {
	// Seed corpus.
	f.Add("event: update\ndata: hello\n\n", "evt-1")
	f.Add("event: update\ndata: hello\nid: old\n\n", "evt-2")
	f.Add("", "some-id")
	f.Add("event: update\ndata: hello\n\n", "")
	f.Add("just plain text", "id-123")
	f.Add("\n\n\n", "x")
	f.Add("data: only data\n\n", "abc")
	f.Add("id: existing\nid: duplicate\nevent: e\ndata: d\n\n", "new-id")

	f.Fuzz(func(t *testing.T, msg, id string) {
		result := injectSSEID(msg, id)

		// No panics is the primary check.

		if id == "" {
			// Should return message unchanged.
			if result != msg {
				t.Errorf("injectSSEID with empty id should return original message")
			}
			return
		}

		// Invariant: result should contain the injected id.
		expected := "id: " + id
		if !strings.Contains(result, expected) {
			t.Errorf("injected message missing id line %q in %q", expected, result)
		}

		// Invariant: should not contain duplicate id lines.
		count := strings.Count(result, "\nid: ") + strings.Count(result, "id: ")
		// Account for possible start-of-string match.
		idLines := 0
		for _, line := range strings.Split(result, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "id:") {
				idLines++
			}
		}
		if idLines > 1 {
			t.Errorf("injected message has %d id lines, want at most 1: %q", idLines, result)
		}
		_ = count // suppress unused
	})
}

func FuzzExtractSSEID(f *testing.F) {
	f.Add("event: update\ndata: hello\nid: evt-1\n\n")
	f.Add("event: update\ndata: hello\n\n")
	f.Add("")
	f.Add("id: standalone")
	f.Add("data: only\nid: mid\nevent: e\n\n")
	f.Add("not sse at all")
	f.Add("\n\n\n\nid: buried\n\n\n")
	f.Add("id:nospace")
	f.Add("id: \n")

	f.Fuzz(func(t *testing.T, msg string) {
		cleaned, id := extractSSEID(msg)

		// No panics is the primary check.

		if id == "" {
			// Should return original message unchanged.
			if cleaned != msg {
				t.Errorf("extractSSEID with no id should return original: got %q, want %q", cleaned, msg)
			}
		} else {
			// The id should have been present in original.
			if !strings.Contains(msg, "id:") {
				t.Errorf("extracted id %q but original message has no id: prefix: %q", id, msg)
			}
			// The cleaned message should not contain the id line.
			for _, line := range strings.Split(cleaned, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "id:") {
					t.Errorf("cleaned message still contains id line: %q", cleaned)
					break
				}
			}
		}
	})
}

func FuzzExtractSSEData(f *testing.F) {
	f.Add("event: update\ndata: hello\n\n")
	f.Add("data: line1\ndata: line2\n\n")
	f.Add("")
	f.Add("no data lines here")
	f.Add("data:\n")
	f.Add("data: \n")
	f.Add("  data: indented\n")
	f.Add("data:nospace")

	f.Fuzz(func(t *testing.T, msg string) {
		result := extractSSEData(msg)
		// No panics is the primary check.
		_ = result
	})
}

func FuzzIsSSEFormatted(f *testing.F) {
	f.Add("event: update\ndata: hello\n\n")
	f.Add("data: just data\n\n")
	f.Add("id: just-id\n")
	f.Add("")
	f.Add("plain text")
	f.Add("event:")
	f.Add("data:")
	f.Add("EVENT: uppercase")
	f.Add("retry: 1000")

	f.Fuzz(func(t *testing.T, msg string) {
		result := isSSEFormatted(msg)
		// Verify consistency with prefix check.
		hasEventPrefix := strings.HasPrefix(msg, "event:")
		hasDataPrefix := strings.HasPrefix(msg, "data:")
		if result != (hasEventPrefix || hasDataPrefix) {
			t.Errorf("isSSEFormatted(%q) = %v, but event prefix = %v, data prefix = %v",
				msg, result, hasEventPrefix, hasDataPrefix)
		}
	})
}

func FuzzIsControlEvent(f *testing.F) {
	f.Add("event: tavern-reconnected\ndata: {}\n\n")
	f.Add("event: tavern-replay-gap\ndata: {}\n\n")
	f.Add("event: update\ndata: hello\n\n")
	f.Add("")
	f.Add("event: tavern-")
	f.Add("event: TAVERN-")

	f.Fuzz(func(t *testing.T, msg string) {
		result := isControlEvent(msg)
		expected := strings.HasPrefix(msg, "event: tavern-")
		if result != expected {
			t.Errorf("isControlEvent(%q) = %v, want %v", msg, result, expected)
		}
	})
}

func FuzzWrapForGroup(f *testing.F) {
	f.Add("chat/room1", "hello world")
	f.Add("chat/room1", "event: update\ndata: hello\n\n")
	f.Add("chat/room1", "event: tavern-reconnected\ndata: {}\n\n")
	f.Add("", "")
	f.Add("a/b/c", "event: e\ndata: d\nid: i\n\n")
	f.Add("topic", "id: injected-id\nraw message")
	f.Add("t", "\n\n\n")
	f.Add("topic/with/many/segments", "data: multiline\ndata: message\n\n")

	f.Fuzz(func(t *testing.T, topic, msg string) {
		result := wrapForGroup(topic, msg)

		// No panics is the primary check.

		if isControlEvent(msg) {
			// Control events pass through unchanged.
			if result != msg {
				t.Errorf("control event was modified: got %q, want %q", result, msg)
			}
			return
		}

		// All non-control results should be valid SSE frames ending with \n\n.
		if !strings.HasSuffix(result, "\n\n") {
			t.Errorf("wrapForGroup result does not end with \\n\\n: %q", result)
		}

		// Should contain event: and data: fields.
		if !strings.Contains(result, "event: ") {
			t.Errorf("wrapForGroup result missing event: field: %q", result)
		}
		if !strings.Contains(result, "data: ") {
			t.Errorf("wrapForGroup result missing data: field: %q", result)
		}
	})
}

func FuzzInjectExtractSSEIDRoundTrip(f *testing.F) {
	f.Add("event: update\ndata: hello\n\n", "evt-1")
	f.Add("event: msg\ndata: line1\ndata: line2\n\n", "id-abc")
	f.Add("data: only\n\n", "123")

	f.Fuzz(func(t *testing.T, msg, id string) {
		if id == "" {
			return
		}
		// SSE IDs cannot contain newlines — they break the wire format.
		// Skip inputs that would produce invalid SSE frames.
		if strings.ContainsAny(id, "\n\r") {
			return
		}
		// An id that trims to empty won't survive the round-trip because
		// extractSSEID returns "" for "id: " (all whitespace).
		want := strings.TrimSpace(id)
		if want == "" {
			return
		}

		injected := injectSSEID(msg, id)
		_, extractedID := extractSSEID(injected)

		if extractedID != want {
			t.Errorf("round-trip failed: injected id %q (trimmed %q), extracted %q from %q",
				id, want, extractedID, injected)
		}
	})
}
