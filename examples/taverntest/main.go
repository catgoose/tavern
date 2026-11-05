// Command taverntest is a tour of the taverntest helpers. Run it to see
// what each helper observes; read tour_test.go for the same steps written
// as tests with the helpers' assertions:
//
//	go run ./examples/taverntest
//	go test ./examples/taverntest
//
// What each helper exercises:
//   - MockBroker records publish calls and delivers nothing. It checks what
//     application code publishes, not delivery.
//   - Capture, Recorder, SimulatedConnection and SlowSubscriber subscribe to
//     a real broker in this process, without HTTP.
//   - SSERecorder is an in-memory ResponseWriter: it parses what a real
//     handler writes, but no network or browser is involved.
//
// None of them is evidence of browser behavior; the task board's browser
// tests are.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

// Publisher is what the notifier needs from a broker. *tavern.SSEBroker
// and *taverntest.MockBroker both implement it.
type Publisher interface {
	Publish(topic, msg string)
	PublishTo(topic, scope, msg string)
}

// notifier is the application code under test: it announces orders to
// everyone and their totals to the team that owns them.
type notifier struct{ pub Publisher }

func (n notifier) orderPlaced(id, team string, total int) {
	n.pub.Publish("orders", "order "+id)
	n.pub.PublishTo("totals", team, fmt.Sprintf("order %s: %d", id, total))
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(w io.Writer) error {
	// MockBroker: what was published, with no delivery.
	mock := taverntest.NewMockBroker()
	notifier{mock}.orderPlaced("7", "red", 42)
	fmt.Fprintln(w, "MockBroker calls (nothing delivered):")
	for _, c := range mock.Calls() {
		fmt.Fprintf(w, "  %+v\n", c)
	}

	b := tavern.NewSSEBroker(tavern.WithBufferSize(4))
	defer b.Close()

	// Capture and scoped Capture: what a real subscriber receives.
	all := taverntest.NewCapture(b, "orders")
	red := taverntest.NewScopedCapture(b, "totals", "red")
	blue := taverntest.NewScopedCapture(b, "totals", "blue")
	notifier{b}.orderPlaced("8", "red", 10)
	time.Sleep(50 * time.Millisecond)
	fmt.Fprintf(w, "Capture orders: %q\n", all.Messages())
	fmt.Fprintf(w, "Capture totals as red: %q; as blue: %q\n", red.Messages(), blue.Messages())
	all.Close()
	red.Close()
	blue.Close()

	// Recorder: messages in order, waiting for a count.
	rec := taverntest.NewRecorder(b, "orders")
	for _, id := range []string{"9", "10", "11"} {
		b.Publish("orders", "order "+id)
	}
	fmt.Fprintf(w, "Recorder got 3 within a second: %t, %q\n", rec.WaitFor(3, time.Second), rec.Messages())
	rec.Close()

	// SSERecorder: the frames a real handler writes, parsed, and its flushes.
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
	fmt.Fprintf(w, "SSERecorder: content type %q, %d flushes, events %+v\n", sse.Header().Get("Content-Type"), sse.FlushCount(), sse.Events())

	// SimulatedConnection: disconnect, miss a message, resume by ID.
	b.SetReplayPolicy("feed", 10)
	conn := taverntest.NewSimulatedConnection(b, "feed")
	b.PublishWithID("feed", "1", "first")
	conn.WaitFor(1, time.Second)
	conn.Disconnect()
	b.PublishWithID("feed", "2", "missed while away")
	conn.Reconnect("1")
	conn.WaitForReconnect(1, time.Second)
	fmt.Fprintf(w, "SimulatedConnection: before %q, after resuming from 1 %q\n", conn.Messages(), conn.ReconnectMessages())
	conn.Close()

	// SlowSubscriber: a reader that falls behind a small queue.
	slow := taverntest.NewSlowSubscriber(b, "ticks", taverntest.SlowSubscriberConfig{ReadDelay: 20 * time.Millisecond})
	for i := range 20 {
		b.Publish("ticks", fmt.Sprint(i))
	}
	time.Sleep(300 * time.Millisecond)
	fmt.Fprintf(w, "SlowSubscriber read %d of 20 (queue of 4); drops counted by the broker: %d\n", slow.Len(), b.PublishDrops())
	slow.Close()
	fmt.Fprintf(w, "subscriptions left: %d\n", b.SubscriberCount())
	return nil
}
