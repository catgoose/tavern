package taverntest_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/catgoose/tavern/taverntest"
)

func TestSSERecorder_ImplementsInterfaces(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	// Verify it satisfies http.ResponseWriter and http.Flusher
	var _ http.ResponseWriter = rec
	var _ http.Flusher = rec
}

func TestSSERecorder_WriteAndBody(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	n, err := rec.Write([]byte("hello "))
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("expected 6 bytes written, got %d", n)
	}

	_, _ = rec.Write([]byte("world"))

	if rec.Body() != "hello world" {
		t.Errorf("unexpected body: %q", rec.Body())
	}
}

func TestSSERecorder_WriteHeader(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	if rec.Code() != http.StatusOK {
		t.Errorf("expected default status 200, got %d", rec.Code())
	}

	rec.WriteHeader(http.StatusNotFound)
	if rec.Code() != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code())
	}
}

func TestSSERecorder_Header(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	rec.Header().Set("Content-Type", "text/event-stream")
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Error("expected Content-Type header to be set")
	}
}

func TestSSERecorder_Flush(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	if rec.FlushCount() != 0 {
		t.Error("expected 0 flushes initially")
	}

	rec.Flush()
	rec.Flush()

	if rec.FlushCount() != 2 {
		t.Errorf("expected 2 flushes, got %d", rec.FlushCount())
	}
}

func TestSSERecorder_ParsesEvents(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	// Write two SSE events
	fmt.Fprint(rec, "event: message\ndata: hello\n\n")
	fmt.Fprint(rec, "event: update\ndata: world\nid: 42\n\n")

	events := rec.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	if events[0].Event != "message" {
		t.Errorf("event[0].Event: expected %q, got %q", "message", events[0].Event)
	}
	if events[0].Data != "hello" {
		t.Errorf("event[0].Data: expected %q, got %q", "hello", events[0].Data)
	}

	if events[1].Event != "update" {
		t.Errorf("event[1].Event: expected %q, got %q", "update", events[1].Event)
	}
	if events[1].Data != "world" {
		t.Errorf("event[1].Data: expected %q, got %q", "world", events[1].Data)
	}
	if events[1].ID != "42" {
		t.Errorf("event[1].ID: expected %q, got %q", "42", events[1].ID)
	}
}

func TestSSERecorder_ParsesMultilineData(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, "event: msg\ndata: line1\ndata: line2\ndata: line3\n\n")

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "line1\nline2\nline3" {
		t.Errorf("expected multiline data, got %q", events[0].Data)
	}
}

func TestSSERecorder_IgnoresComments(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, ": keepalive\nevent: msg\ndata: hello\n\n")

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello" {
		t.Errorf("expected %q, got %q", "hello", events[0].Data)
	}
}

func TestSSERecorder_ParsesRetry(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, "event: msg\ndata: hello\nretry: 3000\n\n")

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Retry != "3000" {
		t.Errorf("expected retry %q, got %q", "3000", events[0].Retry)
	}
}

func TestSSERecorder_AssertEvent(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, "event: greeting\ndata: hello\n\n")
	fmt.Fprint(rec, "event: farewell\ndata: bye\n\n")

	rec.AssertEvent(t, "greeting", "hello")
	rec.AssertEvent(t, "farewell", "bye")
}

func TestSSERecorder_AssertEventCount(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, "event: a\ndata: 1\n\n")
	fmt.Fprint(rec, "event: b\ndata: 2\n\n")
	fmt.Fprint(rec, "event: c\ndata: 3\n\n")

	rec.AssertEventCount(t, 3)
}

func TestSSERecorder_AssertHasID(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	fmt.Fprint(rec, "event: msg\ndata: hello\nid: evt-123\n\n")

	rec.AssertHasID(t, "evt-123")
}

func TestSSERecorder_AssertContentType(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	rec.Header().Set("Content-Type", "text/event-stream")
	rec.AssertContentType(t)
}

func TestSSERecorder_AssertFlushed(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	rec.Flush()
	rec.AssertFlushed(t)
}

func TestSSERecorder_TrailingEventWithoutBlankLine(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	// Event without trailing blank line (edge case)
	fmt.Fprint(rec, "event: msg\ndata: trailing")

	events := rec.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "trailing" {
		t.Errorf("expected %q, got %q", "trailing", events[0].Data)
	}
}

func TestSSERecorder_EmptyBody(t *testing.T) {
	rec := taverntest.NewSSERecorder()

	events := rec.Events()
	if len(events) != 0 {
		t.Errorf("expected 0 events from empty body, got %d", len(events))
	}
}

func TestSSERecorder_WithSSEMessage(t *testing.T) {
	// Test that SSERecorder can parse output from tavern.SSEMessage.String()
	rec := taverntest.NewSSERecorder()

	// This matches the format from tavern.NewSSEMessage("update", "data").WithID("5").String()
	fmt.Fprint(rec, "event: update\ndata: hello world\nid: 5\n\n")

	rec.AssertEvent(t, "update", "hello world")
	rec.AssertHasID(t, "5")
	rec.AssertEventCount(t, 1)
}
