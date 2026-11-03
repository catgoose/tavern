package tavern

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitTermination waits for status to end and returns its cause.
func waitTermination(t *testing.T, status *SubscriptionStatus) Termination {
	t.Helper()
	select {
	case <-status.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("subscription status never ended")
	}
	return status.Termination()
}

// drainExpectingCause reads ch until it closes and checks that the cause
// was already recorded when the close became visible.
func drainExpectingCause(t *testing.T, ch <-chan string, status *SubscriptionStatus) []string {
	t.Helper()
	var got []string
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				require.NotEqual(t, TerminationNone, status.Termination().Reason,
					"cause must be recorded before the data channel closes")
				return got
			}
			got = append(got, msg)
		case <-timeout:
			t.Fatalf("channel still open after reading %v", got)
		}
	}
}

func TestTerminationReasons(t *testing.T) {
	cases := []struct {
		name   string
		opts   []BrokerOption
		sub    []SubscribeOption
		end    func(b *SSEBroker, unsub func())
		reason TerminationReason
	}{
		{name: "unsubscribe", end: func(_ *SSEBroker, unsub func()) { unsub() }, reason: TerminationUnsubscribe},
		{
			name: "disconnect", sub: []SubscribeOption{SubWithMeta(SubscribeMeta{ID: "u1"})},
			end:    func(b *SSEBroker, _ func()) { b.Disconnect("t", "u1") },
			reason: TerminationDisconnect,
		},
		{
			name: "queue loss", opts: []BrokerOption{WithBufferSize(1)}, sub: []SubscribeOption{SubWithDisconnectOnLoss()},
			end:    func(b *SSEBroker, _ func()) { b.Publish("t", "1"); b.Publish("t", "2") },
			reason: TerminationQueueLoss,
		},
		{
			name: "blocking publish timeout", opts: []BrokerOption{WithBufferSize(1)}, sub: []SubscribeOption{SubWithDisconnectOnLoss()},
			end: func(b *SSEBroker, _ func()) {
				b.Publish("t", "1")
				_ = b.PublishBlocking("t", "2", time.Millisecond)
			},
			reason: TerminationQueueLoss,
		},
		{
			name: "slow eviction", opts: []BrokerOption{WithBufferSize(1), WithSlowSubscriberEviction(1)},
			end:    func(b *SSEBroker, _ func()) { b.Publish("t", "1"); b.Publish("t", "2") },
			reason: TerminationEvicted,
		},
		{
			name:   "adaptive disconnect",
			opts:   []BrokerOption{WithBufferSize(1), WithAdaptiveBackpressure(AdaptiveBackpressure{ThrottleAt: 10, SimplifyAt: 20, DisconnectAt: 1})},
			end:    func(b *SSEBroker, _ func()) { b.Publish("t", "1"); b.Publish("t", "2") },
			reason: TerminationEvicted,
		},
		{name: "broker close", end: func(b *SSEBroker, _ func()) { b.Close() }, reason: TerminationBrokerClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewSSEBroker(tc.opts...)
			defer b.Close()
			status := NewSubscriptionStatus()
			assert.Equal(t, TerminationNone, status.Termination().Reason)

			ch, unsub := b.SubscribeWith("t", append(tc.sub, SubWithStatus(status))...)
			tc.end(b, unsub)

			// The cause is observable without reading the (possibly full)
			// data channel.
			got := waitTermination(t, status)
			assert.Equal(t, tc.reason, got.Reason, got.Reason.String())
			assert.Equal(t, "t", got.Topic)
			assert.NoError(t, got.Err)
			drainExpectingCause(t, ch, status)

			// Later teardown keeps the first cause.
			unsub()
			b.Close()
			assert.Equal(t, got, status.Termination())
		})
	}
}

func TestTerminationAtSubscribe(t *testing.T) {
	t.Run("closed broker", func(t *testing.T) {
		b := NewSSEBroker()
		b.Close()
		status := NewSubscriptionStatus()
		ch, _ := b.SubscribeWith("t", SubWithStatus(status))
		drainUntilClosed(t, ch)
		assert.Equal(t, Termination{Reason: TerminationBrokerClosed, Topic: "t"}, waitTermination(t, status))
	})
	t.Run("rejected", func(t *testing.T) {
		b := NewSSEBroker(WithMaxSubscribers(1))
		defer b.Close()
		_, unsub := b.Subscribe("other")
		defer unsub()
		status := NewSubscriptionStatus()
		ch, _ := b.SubscribeFromIDWith("t", "", SubWithStatus(status))
		assert.Nil(t, ch)
		assert.Equal(t, Termination{Reason: TerminationRejected, Topic: "t"}, waitTermination(t, status))
	})
}

func TestTerminationObservableWhileConsumerBlocked(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(2))
	defer b.Close()
	status := NewSubscriptionStatus()
	ch, _ := b.SubscribeWith("t", SubWithDisconnectOnLoss(), SubWithStatus(status))

	// The consumer is blocked elsewhere and never reads.
	for i := 0; i < 3; i++ {
		b.Publish("t", fmt.Sprintf("m%d", i))
	}
	got := waitTermination(t, status)
	assert.Equal(t, TerminationQueueLoss, got.Reason)
	// Messages queued before the loss are still readable after it.
	assert.Equal(t, []string{"m0", "m1"}, drainExpectingCause(t, ch, status))
}

func TestTerminationConcurrentTeardownSingleCause(t *testing.T) {
	allowed := map[TerminationReason]bool{
		TerminationUnsubscribe: true, TerminationDisconnect: true,
		TerminationQueueLoss: true, TerminationBrokerClosed: true,
	}
	for i := 0; i < 50; i++ {
		b := NewSSEBroker(WithBufferSize(1))
		status := NewSubscriptionStatus()
		ch, unsub := b.SubscribeWith("t", SubWithDisconnectOnLoss(), SubWithMeta(SubscribeMeta{ID: "u"}), SubWithStatus(status))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, fn := range []func(){
			unsub,
			func() { b.Disconnect("t", "u") },
			func() { b.Publish("t", "a"); b.Publish("t", "b") },
			b.Close,
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				fn()
			}()
		}
		close(start)
		wg.Wait()
		got := waitTermination(t, status)
		require.True(t, allowed[got.Reason], got.Reason.String())
		drainExpectingCause(t, ch, status)
		unsub()
		assert.Equal(t, got, status.Termination(), "cause is stable")
	}
}

func TestSubscriptionStatusMisuse(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	status := NewSubscriptionStatus()
	_, unsub := b.SubscribeWith("t", SubWithStatus(status))
	defer unsub()
	assert.Panics(t, func() { b.SubscribeWith("t", SubWithStatus(status)) })
	assert.Panics(t, func() { b.SubscribeGlobWith("t/*", SubWithStatus(NewSubscriptionStatus())) })
	assert.Panics(t, func() { b.SubscribeGlobWith("t/*", SubWithTerminateOnReplayError()) })
}

func TestTerminationReasonString(t *testing.T) {
	assert.Equal(t, "queue-loss", TerminationQueueLoss.String())
	assert.Equal(t, "active", TerminationNone.String())
	assert.Equal(t, "unknown", TerminationReason(200).String())
	assert.Equal(t, "after-id", ReplayOpAfterID.String())
	assert.Equal(t, "unknown", ReplayStoreOp(0).String())
}

func TestMultiStatusPreservesTriggeringLoss(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()
	status := NewSubscriptionStatus()
	out, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithDisconnectOnLoss(), SubWithStatus(status))

	// Nobody reads the aggregate, so "a" overflows and is evicted; its
	// cancellation of "b" must not overwrite the cause.
	for i := 0; b.HasSubscribers("a"); i++ {
		require.Less(t, i, 1000, "inner subscription never overflowed")
		b.Publish("a", "msg")
	}
	got := waitTermination(t, status)
	assert.Equal(t, Termination{Reason: TerminationQueueLoss, Topic: "a"}, got)
	require.Eventually(t, func() bool { return !b.HasSubscribers("b") }, 2*time.Second, time.Millisecond)
	drainMultiUntilClosed(t, out)
	unsub()
	b.Close()
	assert.Equal(t, got, status.Termination())
	assertSubscriptionStateReleased(t, b, "a")
}

func TestMultiStatus(t *testing.T) {
	t.Run("aggregate unsubscribe", func(t *testing.T) {
		b := NewSSEBroker()
		defer b.Close()
		status := NewSubscriptionStatus()
		out, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithTerminateOnInnerClose(), SubWithStatus(status))
		unsub()
		drainMultiUntilClosed(t, out)
		assert.Equal(t, Termination{Reason: TerminationUnsubscribe}, waitTermination(t, status))
	})
	t.Run("required topic disconnected", func(t *testing.T) {
		b := NewSSEBroker()
		defer b.Close()
		status := NewSubscriptionStatus()
		out, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithTerminateOnInnerClose(),
			SubWithMeta(SubscribeMeta{ID: "u"}), SubWithStatus(status))
		defer unsub()
		require.True(t, b.Disconnect("b", "u"))
		assert.Equal(t, Termination{Reason: TerminationDisconnect, Topic: "b"}, waitTermination(t, status))
		drainMultiUntilClosed(t, out)
	})
	t.Run("default continues until all end", func(t *testing.T) {
		b := NewSSEBroker()
		status := NewSubscriptionStatus()
		out, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithMeta(SubscribeMeta{ID: "u"}), SubWithStatus(status))
		defer unsub()
		require.True(t, b.Disconnect("a", "u"))
		b.Publish("b", "still-open")
		select {
		case tm := <-out:
			assert.Equal(t, "still-open", tm.Data)
		case <-time.After(time.Second):
			t.Fatal("aggregate did not continue after one inner subscription ended")
		}
		assert.Equal(t, TerminationNone, status.Termination().Reason)
		b.Close()
		assert.Equal(t, Termination{Reason: TerminationBrokerClosed, Topic: "b"}, waitTermination(t, status))
		drainMultiUntilClosed(t, out)
	})
	t.Run("required topic rejected", func(t *testing.T) {
		b := NewSSEBroker(WithMaxSubscribersPerTopic(1))
		defer b.Close()
		_, unsubB := b.Subscribe("b")
		defer unsubB()
		status := NewSubscriptionStatus()
		out, _ := b.SubscribeMultiWith([]string{"a", "b"}, SubWithTerminateOnInnerClose(), SubWithStatus(status))
		assert.Nil(t, out)
		assert.Equal(t, Termination{Reason: TerminationRejected, Topic: "b"}, waitTermination(t, status))
		assert.False(t, b.HasSubscribers("a"))
	})
}

var errStoreDown = errors.New("store down")

// faultyStore wraps MemoryReplayStore and fails the selected operations.
type faultyStore struct {
	*MemoryReplayStore
	mu   sync.Mutex
	fail map[ReplayStoreOp]bool
	// partialAfterID makes a failing AfterID also return found=true and
	// the entries it has.
	partialAfterID bool
	// onlyTopic, when set, limits failures to that topic.
	onlyTopic string
}

func newFaultyStore() *faultyStore {
	return &faultyStore{MemoryReplayStore: NewMemoryReplayStore(), fail: map[ReplayStoreOp]bool{}}
}

func (f *faultyStore) set(op ReplayStoreOp, failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[op] = failing
}

func (f *faultyStore) failing(op ReplayStoreOp, topic string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail[op] && (f.onlyTopic == "" || f.onlyTopic == topic)
}

func (f *faultyStore) Append(ctx context.Context, topic string, e ReplayEntry) error {
	if f.failing(ReplayOpAppend, topic) {
		return errStoreDown
	}
	return f.MemoryReplayStore.Append(ctx, topic, e)
}

func (f *faultyStore) AfterID(ctx context.Context, topic, lastID string, limit int) ([]ReplayEntry, bool, error) {
	if f.failing(ReplayOpAfterID, topic) {
		if f.partialAfterID {
			entries, found, _ := f.MemoryReplayStore.AfterID(ctx, topic, lastID, limit)
			return entries, found, errStoreDown
		}
		return nil, false, errStoreDown
	}
	return f.MemoryReplayStore.AfterID(ctx, topic, lastID, limit)
}

func (f *faultyStore) Latest(ctx context.Context, topic string, limit int) ([]ReplayEntry, error) {
	if f.failing(ReplayOpLatest, topic) {
		return nil, errStoreDown
	}
	return f.MemoryReplayStore.Latest(ctx, topic, limit)
}

func (f *faultyStore) DeleteTopic(ctx context.Context, topic string) error {
	if f.failing(ReplayOpDeleteTopic, topic) {
		return errStoreDown
	}
	return f.MemoryReplayStore.DeleteTopic(ctx, topic)
}

func (f *faultyStore) SetMaxEntries(ctx context.Context, topic string, n int) error {
	if f.failing(ReplayOpSetMaxEntries, topic) {
		return errStoreDown
	}
	return f.MemoryReplayStore.SetMaxEntries(ctx, topic, n)
}

// errorLog collects reported store errors. Its handler also calls back into
// the broker, which deadlocks if the handler runs under a broker lock.
type errorLog struct {
	mu   sync.Mutex
	errs []*ReplayStoreError
}

func (l *errorLog) handler(b **SSEBroker) func(*ReplayStoreError) {
	return func(e *ReplayStoreError) {
		_ = (*b).Stats()
		(*b).Publish("errors", e.Error())
		l.mu.Lock()
		l.errs = append(l.errs, e)
		l.mu.Unlock()
	}
}

func (l *errorLog) ops() []ReplayStoreOp {
	l.mu.Lock()
	defer l.mu.Unlock()
	ops := make([]ReplayStoreOp, len(l.errs))
	for i, e := range l.errs {
		ops[i] = e.Op
	}
	return ops
}

func newFaultyBroker(t *testing.T, opts ...BrokerOption) (*SSEBroker, *faultyStore, *errorLog) {
	t.Helper()
	store := newFaultyStore()
	log := &errorLog{}
	var b *SSEBroker
	b = NewSSEBroker(append([]BrokerOption{WithReplayStore(store), WithReplayStoreErrorHandler(log.handler(&b))}, opts...)...)
	t.Cleanup(b.Close)
	return b, store, log
}

func TestReplayStoreErrorReporting(t *testing.T) {
	b, store, log := newFaultyBroker(t)
	b.SetReplayPolicy("t", 5)
	_, unsub := b.Subscribe("t")
	defer unsub()

	store.set(ReplayOpAppend, true)
	live, unsubLive := b.Subscribe("t")
	defer unsubLive()
	b.PublishWithReplay("t", "replay")
	b.PublishWithID("t", "id-1", "with-id")
	b.PublishWithTTL("t", "ttl", time.Minute)
	b.PublishToWithTTL("t", "s", "scoped-ttl", time.Minute)
	// Live fan-out still happened for every unscoped publish.
	assert.Equal(t, []string{"replay", "with-id", "ttl"}, receiveN(t, live, 3))

	store.set(ReplayOpSetMaxEntries, true)
	b.SetReplayPolicy("t", 3)
	store.set(ReplayOpDeleteTopic, true)
	b.SetReplayPolicy("t", 0)
	b.ClearReplay("t")

	assert.Equal(t, []ReplayStoreOp{
		ReplayOpAppend, ReplayOpAppend, ReplayOpAppend, ReplayOpAppend,
		ReplayOpSetMaxEntries, ReplayOpDeleteTopic, ReplayOpDeleteTopic,
	}, log.ops())
	assert.Equal(t, int64(7), b.Stats().ReplayStoreErrors)
	for _, e := range log.errs {
		assert.ErrorIs(t, e, errStoreDown)
		assert.Equal(t, "t", e.Topic)
	}
}

// receiveN reads n messages, stripping injected SSE id lines.
func receiveN(t *testing.T, ch <-chan string, n int) []string {
	t.Helper()
	var got []string
	for len(got) < n {
		select {
		case msg := <-ch:
			msg, _, _ = strings.Cut(msg, "\nid: ")
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatalf("received %v, want %d messages", got, n)
		}
	}
	return got
}

func TestReplayLatestFailure(t *testing.T) {
	subscribers := map[string]func(b *SSEBroker, opts ...SubscribeOption) (<-chan string, func()){
		"SubscribeWith": func(b *SSEBroker, opts ...SubscribeOption) (<-chan string, func()) {
			return b.SubscribeWith("t", opts...)
		},
		"SubscribeFromIDWith": func(b *SSEBroker, opts ...SubscribeOption) (<-chan string, func()) {
			return b.SubscribeFromIDWith("t", "", opts...)
		},
	}
	for name, subscribe := range subscribers {
		t.Run(name+"/legacy", func(t *testing.T) {
			b, store, log := newFaultyBroker(t)
			b.PublishWithReplay("t", "old")
			store.set(ReplayOpLatest, true)
			status := NewSubscriptionStatus()
			ch, unsub := subscribe(b, SubWithStatus(status))
			defer unsub()
			assert.Equal(t, []ReplayStoreOp{ReplayOpLatest}, log.ops())
			b.Publish("t", "live")
			assert.Equal(t, "live", <-ch, "legacy subscription continues without replay")
			assert.Equal(t, TerminationNone, status.Termination().Reason)
		})
		t.Run(name+"/terminate", func(t *testing.T) {
			b, store, _ := newFaultyBroker(t)
			b.PublishWithReplay("t", "old")
			store.set(ReplayOpLatest, true)
			status := NewSubscriptionStatus()
			ch, unsub := subscribe(b, SubWithTerminateOnReplayError(), SubWithStatus(status))
			defer unsub()
			assert.Empty(t, drainExpectingCause(t, ch, status))
			got := status.Termination()
			assert.Equal(t, TerminationReplayFailure, got.Reason)
			var rse *ReplayStoreError
			require.ErrorAs(t, got.Err, &rse)
			assert.Equal(t, ReplayOpLatest, rse.Op)
			assert.ErrorIs(t, got.Err, errStoreDown)
			assertSubscriptionStateReleased(t, b, "t")
		})
	}
	t.Run("snapshot stays readable", func(t *testing.T) {
		b, store, _ := newFaultyBroker(t)
		store.set(ReplayOpLatest, true)
		status := NewSubscriptionStatus()
		ch, _ := b.SubscribeFromIDWith("t", "", SubWithSnapshot(func() string { return "snap" }),
			SubWithTerminateOnReplayError(), SubWithStatus(status))
		assert.Equal(t, []string{"snap"}, drainExpectingCause(t, ch, status))
		assert.Equal(t, TerminationReplayFailure, status.Termination().Reason)
	})
}

func TestReplayAfterIDFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v/legacy", partial), func(t *testing.T) {
			b, store, log := newFaultyBroker(t)
			b.SetReplayPolicy("t", 5)
			b.SetReplayGapPolicy("t", GapFallbackToSnapshot, func() string { return "gap-snap" })
			b.PublishWithID("t", "1", "one")
			b.PublishWithID("t", "2", "two")
			store.partialAfterID = partial
			store.set(ReplayOpAfterID, true)

			ch, unsub := b.SubscribeFromIDWith("t", "1")
			defer unsub()
			assert.Equal(t, []ReplayStoreOp{ReplayOpAfterID}, log.ops())
			first := <-ch
			if partial {
				// Legacy delivers what the failing store returned.
				assert.Contains(t, first, "two")
			} else {
				// Legacy treats the failure as a Last-Event-ID gap.
				assert.Contains(t, first, "tavern-replay-gap")
			}
		})
		t.Run(fmt.Sprintf("partial=%v/terminate", partial), func(t *testing.T) {
			b, store, _ := newFaultyBroker(t)
			b.SetReplayPolicy("t", 5)
			b.SetReplayGapPolicy("t", GapFallbackToSnapshot, func() string { return "gap-snap" })
			b.PublishWithID("t", "1", "one")
			b.PublishWithID("t", "2", "two")
			store.partialAfterID = partial
			store.set(ReplayOpAfterID, true)
			gaps := make(chan string, 1)
			b.OnReplayGap("t", func(_ *SubscriberInfo, id string) { gaps <- id })
			reconnects := make(chan ReconnectInfo, 1)
			b.OnReconnect("t", func(info ReconnectInfo) { reconnects <- info })

			status := NewSubscriptionStatus()
			ch, unsub := b.SubscribeFromIDWith("t", "1", SubWithTerminateOnReplayError(), SubWithStatus(status))
			defer unsub()
			// No replay, gap event, snapshot or reconnect status is delivered.
			assert.Empty(t, drainExpectingCause(t, ch, status))
			got := status.Termination()
			assert.Equal(t, TerminationReplayFailure, got.Reason)
			var rse *ReplayStoreError
			require.ErrorAs(t, got.Err, &rse)
			assert.Equal(t, ReplayOpAfterID, rse.Op)
			select {
			case <-gaps:
				t.Fatal("store failure reported as a replay gap")
			case <-reconnects:
				t.Fatal("store failure reported as a reconnect")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestReplayFailureEndsRequiredAggregate(t *testing.T) {
	b, store, _ := newFaultyBroker(t)
	store.onlyTopic = "b"
	store.set(ReplayOpLatest, true)
	status := NewSubscriptionStatus()
	out, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithTerminateOnInnerClose(),
		SubWithTerminateOnReplayError(), SubWithStatus(status))
	defer unsub()
	got := waitTermination(t, status)
	assert.Equal(t, TerminationReplayFailure, got.Reason)
	assert.Equal(t, "b", got.Topic)
	assert.ErrorIs(t, got.Err, errStoreDown)
	drainMultiUntilClosed(t, out)
	assert.False(t, b.HasSubscribers("a"), "the healthy sibling is cancelled")
}
