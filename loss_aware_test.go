package tavern

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainUntilClosed reads ch until it closes and returns what it read. It
// fails the test if the channel stays open.
func drainUntilClosed(t *testing.T, ch <-chan string) []string {
	t.Helper()
	var got []string
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, msg)
		case <-timeout:
			t.Fatalf("channel still open after reading %v", got)
		}
	}
}

func drainMultiUntilClosed(t *testing.T, ch <-chan TopicMessage) []TopicMessage {
	t.Helper()
	var got []TopicMessage
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, msg)
		case <-timeout:
			t.Fatalf("aggregate still open after reading %v", got)
		}
	}
}

func TestDisconnectOnLoss_LaterSuccessCannotEraseLoss(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10), WithSlowSubscriberEviction(64))
	defer b.Close()

	lossAware, unsubLoss := b.SubscribeWith("records", SubWithDisconnectOnLoss())
	defer unsubLoss()
	ordinary, unsubOrdinary := b.Subscribe("records")
	defer unsubOrdinary()

	// Nobody drains while the burst is published.
	for range 10 {
		b.Publish("records", "groups")
	}
	b.Publish("records", "users") // lost: both queues are full
	for range 10 {
		<-ordinary
	}
	b.Publish("records", "groups") // succeeds for the ordinary subscriber

	got := drainUntilClosed(t, lossAware)
	assert.Len(t, got, 10, "buffered messages stay readable before closure")
	assert.NotContains(t, got, "users")
	assert.Equal(t, 1, b.TopicCounts()["records"], "only the ordinary subscriber remains")

	// The ordinary subscriber keeps best-effort behavior: still open.
	select {
	case msg, ok := <-ordinary:
		require.True(t, ok)
		assert.Equal(t, "groups", msg)
	case <-time.After(time.Second):
		t.Fatal("ordinary subscriber did not receive later publish")
	}
	assert.Equal(t, int64(2), b.PublishDrops())
}

func TestDisconnectOnLoss_NoLossStaysOpen(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(4))
	defer b.Close()

	ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss())
	defer unsub()

	for i := range 20 {
		b.Publish("t", fmt.Sprintf("m%d", i))
		assert.Equal(t, fmt.Sprintf("m%d", i), <-ch)
	}
	assert.True(t, b.HasSubscribers("t"))
}

func TestDisconnectOnLoss_ScopedAndFiltered(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeWith("t",
		SubWithScope("u1"),
		SubWithFilter(func(msg string) bool { return strings.HasPrefix(msg, "keep") }),
		SubWithDisconnectOnLoss(),
	)
	defer unsub()

	b.PublishTo("t", "u1", "keep-1")
	// Filtered, other-scope and unscoped publishes are not losses.
	b.PublishTo("t", "u1", "skip")
	b.PublishTo("t", "u2", "keep-other")
	b.Publish("t", "keep-unscoped")
	assert.True(t, b.HasSubscribers("t"))

	b.PublishTo("t", "u1", "keep-2") // queue full: lost
	assert.Equal(t, []string{"keep-1"}, drainUntilClosed(t, ch))
	assert.False(t, b.HasSubscribers("t"))
}

func TestDisconnectOnLoss_SnapshotStaysFirst(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(2))
	defer b.Close()

	ch, unsub := b.SubscribeWith("t",
		SubWithSnapshot(func() string { return "snap" }),
		SubWithMeta(SubscribeMeta{ID: "sess-1"}),
		SubWithDisconnectOnLoss(),
	)
	defer unsub()

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)
	assert.Equal(t, "sess-1", subs[0].ID)

	b.Publish("t", "live-1")
	b.Publish("t", "live-2") // snapshot + live-1 fill the queue
	assert.Equal(t, []string{"snap", "live-1"}, drainUntilClosed(t, ch))
}

func TestDisconnectOnLoss_DropOldestKeepsBufferedMessages(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(2), WithDropOldest())
	defer b.Close()

	lossAware, unsubLoss := b.SubscribeWith("t", SubWithDisconnectOnLoss())
	defer unsubLoss()
	ordinary, unsubOrdinary := b.Subscribe("t")
	defer unsubOrdinary()

	b.Publish("t", "a")
	b.Publish("t", "b")
	b.Publish("t", "c")

	// The loss-aware queue is not rewritten; the new message is the loss.
	assert.Equal(t, []string{"a", "b"}, drainUntilClosed(t, lossAware))
	// Drop-oldest still applies to ordinary subscribers.
	assert.Equal(t, "b", <-ordinary)
	assert.Equal(t, "c", <-ordinary)
}

func TestDisconnectOnLoss_BypassesAdaptiveBackpressure(t *testing.T) {
	b := NewSSEBroker(
		WithBufferSize(1),
		WithAdaptiveBackpressure(AdaptiveBackpressure{ThrottleAt: 1, SimplifyAt: 2, DisconnectAt: 100}),
	)
	defer b.Close()
	b.SetSimplifiedRenderer("t", func(string) string { return "simplified" })

	ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss())
	defer unsub()

	b.Publish("t", "a")
	b.Publish("t", "b") // first loss terminates; no tier events are injected
	assert.Equal(t, []string{"a"}, drainUntilClosed(t, ch))
}

func TestDisconnectOnLoss_PublishBlockingTimeout(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss())
	defer unsub()

	require.NoError(t, b.PublishBlocking("t", "a", 10*time.Millisecond))
	require.ErrorIs(t, b.PublishBlocking("t", "b", 10*time.Millisecond), ErrPublishTimeout)
	assert.Equal(t, []string{"a"}, drainUntilClosed(t, ch))
}

func TestDisconnectOnLoss_EvictionReportedOnce(t *testing.T) {
	var evictions atomic.Int32
	b := NewSSEBroker(WithBufferSize(1), WithSlowSubscriberCallback(func(string) {
		evictions.Add(1)
	}))
	defer b.Close()

	ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss())

	b.Publish("t", "a")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { b.Publish("t", "lost") })
	}
	wg.Wait()
	drainUntilClosed(t, ch)
	unsub()
	unsub()

	require.Eventually(t, func() bool { return evictions.Load() >= 1 }, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), evictions.Load())
}

func TestDisconnectOnLoss_FromIDWithResume(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(4))
	defer b.Close()
	b.SetReplayPolicy("t", 10)
	b.PublishWithID("t", "1", "one")
	b.PublishWithID("t", "2", "two")

	ch, unsub := b.SubscribeFromIDWith("t", "1", SubWithDisconnectOnLoss())
	defer unsub()

	// Replay of "two" plus the tavern-reconnected control event.
	require.Len(t, ch, 2)
	b.PublishWithID("t", "3", "three")
	b.PublishWithID("t", "4", "four")
	b.PublishWithID("t", "5", "five") // lost
	got := drainUntilClosed(t, ch)
	require.Len(t, got, 4)
	assert.Contains(t, got[0], "two")
	assert.Contains(t, got[1], "tavern-reconnected")
	assert.Contains(t, got[3], "four")
}

func TestDisconnectOnLoss_RejectsLossyCombinations(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	rate := SubWithRate(Rate{MaxPerSecond: 1})
	assert.PanicsWithValue(t, "tavern: SubWithDisconnectOnLoss cannot be combined with SubWithRate", func() {
		b.SubscribeWith("t", rate, SubWithDisconnectOnLoss())
	})
	assert.Panics(t, func() {
		b.SubscribeFromIDWith("t", "", rate, SubWithDisconnectOnLoss())
	})
	assert.Panics(t, func() {
		b.SubscribeMultiWith([]string{"a", "b"}, rate, SubWithDisconnectOnLoss())
	})
	assert.PanicsWithValue(t, "tavern: SubscribeGlobWith does not support SubWithDisconnectOnLoss", func() {
		b.SubscribeGlobWith("a/*", SubWithDisconnectOnLoss())
	})
	assert.Equal(t, 0, b.SubscriberCount(), "rejected combinations register nothing")
}

func TestDisconnectOnLoss_ClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss())
	_, ok := <-ch
	assert.False(t, ok)
	unsub()

	multi, unsubMulti := b.SubscribeMultiWith([]string{"a", "b"}, SubWithTerminateOnInnerClose())
	drainMultiUntilClosed(t, multi)
	unsubMulti()
}

func TestTerminateOnInnerClose_DefaultAggregateContinues(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithMeta(SubscribeMeta{ID: "s"}))
	defer unsub()

	b.Disconnect("a", "s")
	b.Publish("b", "still-here")
	select {
	case msg, ok := <-ch:
		require.True(t, ok)
		assert.Equal(t, TopicMessage{Topic: "b", Data: "still-here"}, msg)
	case <-time.After(time.Second):
		t.Fatal("default aggregate stopped after one inner subscription closed")
	}
}

func TestTerminateOnInnerClose_ClosesAggregateAndSiblings(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith([]string{"a", "b"},
		SubWithMeta(SubscribeMeta{ID: "s"}),
		SubWithTerminateOnInnerClose(),
	)

	b.Publish("b", "before")
	require.Equal(t, TopicMessage{Topic: "b", Data: "before"}, <-ch)

	b.Disconnect("a", "s")
	drainMultiUntilClosed(t, ch)
	assert.False(t, b.HasSubscribers("b"), "sibling subscription released")

	done := make(chan struct{})
	go func() {
		unsub()
		unsub()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unsubscribe blocked after termination")
	}
}

func TestTerminateOnInnerClose_AggregateFullStillTerminates(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithDisconnectOnLoss())
	defer unsub()

	// Nobody reads the aggregate. Publish until topic "a" overflows: the
	// aggregate buffer fills, the "a" worker blocks on it, then the inner
	// queue fills and the next publish is a loss.
	for i := 0; b.HasSubscribers("a"); i++ {
		require.Less(t, i, 1000, "inner subscription never overflowed")
		b.Publish("a", fmt.Sprintf("a%d", i))
	}

	// The blocked worker must observe the closure and cancel the sibling
	// without anyone draining the aggregate.
	require.Eventually(t, func() bool { return !b.HasSubscribers("b") }, 2*time.Second, time.Millisecond)
	got := drainMultiUntilClosed(t, ch)
	assert.LessOrEqual(t, len(got), 1, "only the aggregate buffer remains")
}

func TestTerminateOnInnerClose_AdmissionFailureReleasesAll(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribersPerTopic(1))
	defer b.Close()

	_, unsubB := b.Subscribe("b")
	defer unsubB()

	ch, unsub := b.SubscribeMultiWith([]string{"a", "b", "c"}, SubWithTerminateOnInnerClose())
	assert.Nil(t, ch)
	assert.Nil(t, unsub)
	assert.False(t, b.HasSubscribers("a"), "already-admitted topics are released")
	assert.False(t, b.HasSubscribers("c"))

	// Without the policy, admitted topics are still served.
	ch, unsub = b.SubscribeMultiWith([]string{"a", "b"})
	require.NotNil(t, ch)
	defer unsub()
	assert.True(t, b.HasSubscribers("a"))
}

// blockingWriter is an http.ResponseWriter whose writes block until release
// is closed. entered receives a value each time a write starts.
type blockingWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	body    strings.Builder
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Flush()              {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func TestDisconnectOnLoss_BlockedHTTPWriterRecovers(t *testing.T) {
	const buffer = 4
	b := NewSSEBroker(WithBufferSize(buffer))
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith([]string{"users", "groups"}, SubWithDisconnectOnLoss())
	defer unsub()

	w := &blockingWriter{
		header:  http.Header{},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- StreamSSE(t.Context(), w, ch, func(tm TopicMessage) string {
			return NewSSEMessage(tm.Topic, tm.Data).String()
		})
	}()

	b.Publish("users", "u0")
	<-w.entered // the stream is now blocked inside a write

	// Burst distinct keys until an inner queue overflows.
	for i := 0; b.HasSubscribers("users") && b.HasSubscribers("groups"); i++ {
		require.Less(t, i, 1000, "burst never overflowed")
		b.Publish("users", fmt.Sprintf("u%d", i+1))
		b.Publish("groups", fmt.Sprintf("g%d", i+1))
	}
	require.Eventually(t, func() bool {
		return !b.HasSubscribers("users") && !b.HasSubscribers("groups")
	}, 2*time.Second, time.Millisecond)

	// Pending state is bounded by the aggregate buffer.
	assert.LessOrEqual(t, len(ch), buffer)

	close(w.release)
	select {
	case err := <-streamDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after the writer unblocked")
	}
}

func TestDisconnectOnLoss_ConcurrentLifecycle(t *testing.T) {
	baseline := runtime.NumGoroutine()
	b := NewSSEBroker(WithBufferSize(2))

	stop := make(chan struct{})
	var publishers sync.WaitGroup
	for _, topic := range []string{"a", "b"} {
		publishers.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				b.Publish(topic, fmt.Sprintf("%s%d", topic, i))
				b.PublishTo(topic, "s", "scoped")
			}
		})
	}

	var subscribers sync.WaitGroup
	for i := range 20 {
		subscribers.Go(func() {
			for range 20 {
				var unsub func()
				if i%2 == 0 {
					var ch <-chan TopicMessage
					ch, unsub = b.SubscribeMultiWith([]string{"a", "b"}, SubWithDisconnectOnLoss())
					if ch == nil {
						return
					}
					select {
					case <-ch:
					default:
					}
				} else {
					var ch <-chan string
					ch, unsub = b.SubscribeWith("a", SubWithScope("s"), SubWithDisconnectOnLoss())
					if ch == nil {
						return
					}
					select {
					case <-ch:
					default:
					}
				}
				var wg sync.WaitGroup
				wg.Go(unsub)
				wg.Go(unsub)
				wg.Wait()
			}
		})
	}

	time.AfterFunc(20*time.Millisecond, b.Close)
	subscribers.Wait()
	close(stop)
	publishers.Wait()
	b.Close()

	// Fan-in workers and aggregate closers have all exited. Poll inline:
	// assert.Eventually would add goroutines of its own.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("leaked goroutines: %d > %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}
