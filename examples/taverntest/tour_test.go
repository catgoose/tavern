package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestTourRuns(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out.String())
	for _, want := range []string{
		`Capture orders: ["order 8"]`,
		`Capture totals as red: ["order 8: 10"]; as blue: []`,
		`Recorder got 3 within a second: true, ["order 9" "order 10" "order 11"]`,
		`content type "text/event-stream"`,
		`{Event:order Data:order 12 ID:12 Retry:}`,
		`after resuming from 1 ["missed while away\nid: 2" "event: tavern-reconnected`,
		"subscriptions left: 0",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// The same steps as tests, with the helpers' assertions.

func TestNotifierWithMockBroker(t *testing.T) {
	mock := taverntest.NewMockBroker()
	notifier{mock}.orderPlaced("7", "red", 42)
	mock.AssertPublished(t, "orders", "order 7")
	mock.AssertPublishedCount(t, "totals", 1)
	mock.AssertNotPublished(t, "audit")
}

func TestNotifierDeliversToTheTeam(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()
	red := taverntest.NewScopedCapture(b, "totals", "red")
	defer red.Close()
	blue := taverntest.NewScopedCapture(b, "totals", "blue")
	defer blue.Close()
	notifier{b}.orderPlaced("8", "red", 10)
	red.AssertReceivedWithin(t, time.Second, "order 8: 10")
	time.Sleep(50 * time.Millisecond)
	if got := blue.Messages(); len(got) != 0 {
		t.Fatalf("blue received %q", got)
	}
}

func TestRecorderKeepsOrder(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()
	rec := taverntest.NewRecorder(b, "orders")
	defer rec.Close()
	for _, id := range []string{"1", "2", "3"} {
		b.Publish("orders", "order "+id)
	}
	if !rec.WaitFor(3, time.Second) {
		t.Fatal("not all messages arrived")
	}
	rec.AssertCount(t, 3)
	rec.AssertNthMessage(t, 0, "order 1")
	rec.AssertContains(t, "order 3")
}

func TestHandlerWritesEvents(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()
	sse := taverntest.NewSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.SSEHandler("orders").ServeHTTP(sse, httptest.NewRequestWithContext(ctx, "GET", "/", nil))
	}()
	time.Sleep(50 * time.Millisecond)
	b.Publish("orders", tavern.NewSSEMessage("order", "order 12").WithID("12").String())
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	sse.AssertContentType(t)
	sse.AssertFlushed(t)
	sse.AssertEventCount(t, 1)
	sse.AssertEvent(t, "order", "order 12")
	sse.AssertHasID(t, "12")
}

func TestReconnectResumes(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()
	b.SetReplayPolicy("feed", 10)
	conn := taverntest.NewSimulatedConnection(b, "feed")
	defer conn.Close()
	b.PublishWithID("feed", "1", "first")
	conn.WaitFor(1, time.Second)
	conn.Disconnect()
	b.PublishWithID("feed", "2", "missed while away")
	conn.Reconnect("1")
	if !conn.WaitForReconnect(1, time.Second) {
		t.Fatal("nothing replayed")
	}
	// A raw message is replayed with its ID appended, then the broker
	// confirms the resume.
	conn.AssertReceivedAfterReconnect(t, "missed while away\nid: 2")
}

func TestSlowSubscriberFallsBehind(t *testing.T) {
	b := tavern.NewSSEBroker(tavern.WithBufferSize(4))
	defer b.Close()
	slow := taverntest.NewSlowSubscriber(b, "ticks", taverntest.SlowSubscriberConfig{ReadDelay: 20 * time.Millisecond})
	for i := range 20 {
		b.Publish("ticks", "tick")
		_ = i
	}
	time.Sleep(300 * time.Millisecond)
	slow.Close()
	<-slow.Done()
	if slow.Len() >= 20 || b.PublishDrops() == 0 {
		t.Fatalf("read %d, drops %d: a slow reader should fall behind", slow.Len(), b.PublishDrops())
	}
}
