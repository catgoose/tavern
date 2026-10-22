package taverntest_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestCapture_AssertReceived(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "alpha")
	b.Publish("events", "beta")
	b.Publish("events", "gamma")

	capt.AssertReceived(t, "alpha", "gamma") // order-independent
}

func TestCapture_AssertReceivedWithin(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "fast")

	capt.AssertReceivedWithin(t, 500*time.Millisecond, "fast")
}

// fakeTB records failures so assertion failures can be inspected without
// failing the enclosing test. Fatalf ends the calling goroutine like
// testing.T, so assertions must run through run.
type fakeTB struct {
	testing.TB
	mu       sync.Mutex
	failures []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

func (f *fakeTB) run(fn func(testing.TB)) []string {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(f)
	}()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

// publishAfterUnrelated collects alpha and beta, then publishes gamma once
// the assertion is already waiting, so a count of two is reached before the
// required gamma arrives.
func publishAfterUnrelated(t *testing.T, b *tavern.SSEBroker, capt *taverntest.Capture) {
	t.Helper()
	b.Publish("events", "alpha")
	b.Publish("events", "beta")
	deadline := time.Now().Add(time.Second)
	for len(capt.Messages()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("alpha and beta were not collected")
		}
		time.Sleep(time.Millisecond)
	}
	time.AfterFunc(20*time.Millisecond, func() { b.Publish("events", "gamma") })
}

func TestCapture_AssertReceivedWaitsForLateExpectedMessage(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	publishAfterUnrelated(t, b, capt)

	failures := (&fakeTB{}).run(func(tb testing.TB) {
		capt.AssertReceived(tb, "alpha", "gamma")
	})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %q", failures)
	}
}

func TestCapture_AssertReceivedWithinWaitsForLateExpectedMessage(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	publishAfterUnrelated(t, b, capt)

	failures := (&fakeTB{}).run(func(tb testing.TB) {
		capt.AssertReceivedWithin(tb, 2*time.Second, "alpha", "gamma")
	})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %q", failures)
	}
}

func TestCapture_AssertReceivedWithinTimeoutNamesMissingMessages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "alpha")
	b.Publish("events", "beta")

	failures := (&fakeTB{}).run(func(tb testing.TB) {
		capt.AssertReceivedWithin(tb, 50*time.Millisecond, "alpha", "gamma")
	})
	if len(failures) != 1 {
		t.Fatalf("expected one failure, got %q", failures)
	}
	if !strings.Contains(failures[0], `messages ["gamma"]`) {
		t.Errorf("failure should name only the missing message: %s", failures[0])
	}
}

func TestCapture_AssertReceivedEmptyExpectation(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	capt.AssertReceived(t)
	capt.AssertReceivedWithin(t, time.Millisecond)
}

func TestCapture_AssertReceivedDuplicateExpectation(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "alpha")

	capt.AssertReceived(t, "alpha", "alpha")
}

func TestCapture_AssertReceivedExactly(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "first")
	b.Publish("events", "second")

	capt.AssertReceivedExactly(t, "first", "second")
}

func TestCapture_Messages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")
	defer capt.Close()

	b.Publish("events", "msg1")
	b.Publish("events", "msg2")

	capt.AssertReceived(t, "msg1", "msg2")

	msgs := capt.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// Mutating returned slice should not affect capture
	msgs[0] = "mutated"
	fresh := capt.Messages()
	if fresh[0] != "msg1" {
		t.Error("Messages() should return a copy")
	}
}

func TestCapture_ScopedCapture(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	cap1 := taverntest.NewScopedCapture(b, "notif", "user-1")
	defer cap1.Close()
	cap2 := taverntest.NewScopedCapture(b, "notif", "user-2")
	defer cap2.Close()

	b.PublishTo("notif", "user-1", "for-1")
	b.PublishTo("notif", "user-2", "for-2")

	cap1.AssertReceived(t, "for-1")
	cap2.AssertReceived(t, "for-2")
}

func TestCapture_Close(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	capt := taverntest.NewCapture(b, "events")

	b.Publish("events", "before")
	capt.AssertReceived(t, "before")

	capt.Close()

	// Publishing after close should not panic or affect capture
	b.Publish("events", "after")
	time.Sleep(50 * time.Millisecond)

	msgs := capt.Messages()
	if len(msgs) != 1 {
		t.Errorf("expected 1 message after close, got %d", len(msgs))
	}
}
