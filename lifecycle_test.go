package tavern

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/catgoose/tavern/backend"
	"github.com/catgoose/tavern/backend/memory"
)

// lifecycleVariant is one public way to create a single-topic subscription.
type lifecycleVariant struct {
	name      string
	subscribe func(b *SSEBroker, topic string) (<-chan string, func())
	// scope is the scope to publish with; empty for unscoped variants.
	scope string
	// id is the subscriber ID usable with Disconnect; empty when the variant
	// cannot carry metadata.
	id string
	// evictable reports whether a full buffer can evict the subscriber.
	evictable bool
}

func acceptAll(string) bool { return true }

func snapshotValue() string { return "snap" }

func lifecycleVariants() []lifecycleVariant {
	meta := SubscribeMeta{ID: "sub-1"}
	rate := Rate{MinInterval: time.Millisecond}
	return []lifecycleVariant{
		{name: "Subscribe", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.Subscribe(topic)
		}},
		{name: "SubscribeScoped", scope: "s", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeScoped(topic, "s")
		}},
		{name: "SubscribeWithFilter", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWithFilter(topic, acceptAll)
		}},
		{name: "SubscribeScopedWithFilter", scope: "s", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeScopedWithFilter(topic, "s", acceptAll)
		}},
		{name: "SubscribeWithSnapshot", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWithSnapshot(topic, snapshotValue)
		}},
		{name: "SubscribeWithMeta", id: meta.ID, evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWithMeta(topic, meta)
		}},
		{name: "SubscribeWith/scope+snapshot+meta", scope: "s", id: meta.ID, evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWith(topic, SubWithScope("s"), SubWithSnapshot(snapshotValue), SubWithMeta(meta))
		}},
		{name: "SubscribeWith/filter+snapshot+meta", id: meta.ID, evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWith(topic, SubWithFilter(acceptAll), SubWithSnapshot(snapshotValue), SubWithMeta(meta))
		}},
		{name: "SubscribeWith/scope+filter+snapshot+meta", scope: "s", id: meta.ID, evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWith(topic, SubWithScope("s"), SubWithFilter(acceptAll), SubWithSnapshot(snapshotValue), SubWithMeta(meta))
		}},
		{name: "SubscribeWith/rate+meta", id: meta.ID, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWith(topic, SubWithRate(rate), SubWithMeta(meta))
		}},
		{name: "SubscribeWith/disconnectOnLoss+meta", id: meta.ID, evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWith(topic, SubWithDisconnectOnLoss(), SubWithMeta(meta))
		}},
		{name: "SubscribeFromID", evictable: true, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeFromID(topic, "")
		}},
		{name: "SubscribeFromIDWith/scope+rate+meta", scope: "s", id: meta.ID, subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeFromIDWith(topic, "", SubWithScope("s"), SubWithRate(rate), SubWithMeta(meta))
		}},
		{name: "SubscribeWithRate", subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWithRate(topic, rate)
		}},
		{name: "SubscribeScopedWithRate", scope: "s", subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeScopedWithRate(topic, "s", rate)
		}},
		{name: "SubscribeWithCoalescing", subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeWithCoalescing(topic)
		}},
		{name: "SubscribeScopedWithCoalescing", scope: "s", subscribe: func(b *SSEBroker, topic string) (<-chan string, func()) {
			return b.SubscribeScopedWithCoalescing(topic, "s")
		}},
	}
}

func publishFor(b *SSEBroker, v lifecycleVariant, topic, msg string) {
	if v.scope != "" {
		b.PublishTo(topic, v.scope, msg)
		return
	}
	b.Publish(topic, msg)
}

// receiveValue reads ch until want arrives, skipping snapshot and replay
// values delivered first.
func receiveValue(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case got, ok := <-ch:
			require.True(t, ok, "channel closed before %q arrived", want)
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("did not receive %q", want)
		}
	}
}

// assertSubscriptionStateReleased checks that no per-subscriber state
// remains after every subscriber has gone.
func assertSubscriptionStateReleased(t *testing.T, b *SSEBroker, topic string) {
	t.Helper()
	b.mu.RLock()
	assert.Zero(t, len(b.topics[topic])+len(b.scopedTopics[topic]), "registered subscribers")
	assert.Empty(t, b.subscriberMeta, "subscriber metadata")
	assert.Empty(t, b.filterPredicates, "filter predicates")
	assert.Empty(t, b.coalescingChans, "coalescing state")
	b.mu.RUnlock()
	b.dropCountsMu.RLock()
	assert.Empty(t, b.dropCounts, "drop counters")
	b.dropCountsMu.RUnlock()
	if b.adaptive != nil {
		b.adaptive.mu.RLock()
		assert.Empty(t, b.adaptive.states, "adaptive state")
		b.adaptive.mu.RUnlock()
	}
	b.rateLimit.mu.Lock()
	assert.Empty(t, b.rateLimit.subs, "rate limiter entries")
	b.rateLimit.mu.Unlock()
	guards := 0
	b.chanGuards.Range(func(_, _ any) bool { guards++; return true })
	assert.Zero(t, guards, "channel guards")
}

type removalPath struct {
	name string
	// applies reports whether the path can remove v.
	applies func(v lifecycleVariant) bool
	remove  func(t *testing.T, b *SSEBroker, v lifecycleVariant, topic string, unsub func())
	// firesLastHook reports whether OnLastUnsubscribe runs for this path.
	firesLastHook bool
}

func removalPaths() []removalPath {
	always := func(lifecycleVariant) bool { return true }
	return []removalPath{
		{name: "unsubscribe", applies: always, firesLastHook: true, remove: func(_ *testing.T, _ *SSEBroker, _ lifecycleVariant, _ string, unsub func()) {
			unsub()
		}},
		{name: "Disconnect", applies: func(v lifecycleVariant) bool { return v.id != "" }, firesLastHook: true,
			remove: func(t *testing.T, b *SSEBroker, v lifecycleVariant, topic string, _ func()) {
				require.True(t, b.Disconnect(topic, v.id))
			}},
		{name: "eviction", applies: func(v lifecycleVariant) bool { return v.evictable }, firesLastHook: true,
			remove: func(t *testing.T, b *SSEBroker, v lifecycleVariant, topic string, _ func()) {
				for i := 0; b.HasSubscribers(topic); i++ {
					require.Less(t, i, 100, "subscriber was never evicted")
					publishFor(b, v, topic, fmt.Sprintf("fill-%d", i))
				}
			}},
		{name: "Close", applies: always, remove: func(_ *testing.T, b *SSEBroker, _ lifecycleVariant, _ string, _ func()) {
			b.Close()
		}},
	}
}

type lifecycleBroker struct {
	name string
	opts func(be backend.Backend) []BrokerOption
}

func lifecycleBrokers() []lifecycleBroker {
	return []lifecycleBroker{
		{name: "eviction", opts: func(be backend.Backend) []BrokerOption {
			return []BrokerOption{WithBufferSize(2), WithSlowSubscriberEviction(1), WithBackend(be)}
		}},
		{name: "adaptive", opts: func(be backend.Backend) []BrokerOption {
			return []BrokerOption{WithBufferSize(2), WithBackend(be), WithAdaptiveBackpressure(AdaptiveBackpressure{
				ThrottleAt: 1, SimplifyAt: 2, DisconnectAt: 3,
			})}
		}},
	}
}

// TestLifecycleContract checks every single-topic subscription variant
// against every applicable removal path: the channel closes, per-subscriber
// state is released, lifecycle hooks and backend registration follow the
// first and last subscriber, and repeat unsubscribe stays safe.
func TestLifecycleContract(t *testing.T) {
	for _, cfg := range lifecycleBrokers() {
		for _, v := range lifecycleVariants() {
			for _, path := range removalPaths() {
				if !path.applies(v) {
					continue
				}
				t.Run(cfg.name+"/"+v.name+"/"+path.name, func(t *testing.T) {
					const topic = "orders"
					mem := memory.New()
					defer mem.Close()
					counting := newCountingBackend(mem)
					b := NewSSEBroker(cfg.opts(counting)...)
					defer b.Close()

					first := make(chan struct{}, 1)
					last := make(chan struct{}, 1)
					b.OnFirstSubscriber(topic, func(string) { first <- struct{}{} })
					b.OnLastUnsubscribe(topic, func(string) { last <- struct{}{} })

					ch, unsub := v.subscribe(b, topic)
					require.NotNil(t, ch)
					select {
					case <-first:
					case <-time.After(time.Second):
						t.Fatal("OnFirstSubscriber did not fire")
					}
					assert.Equal(t, 1, counting.activeFor(topic), "backend subscribed on first subscriber")
					if v.name != "SubscribeWithCoalescing" && v.name != "SubscribeScopedWithCoalescing" {
						assert.Len(t, b.Subscribers(topic), 1, "subscriber metadata registered")
					}

					// Leave one undelivered message queued so removal
					// happens with a non-empty buffer.
					publishFor(b, v, topic, "queued")
					path.remove(t, b, v, topic, unsub)

					drainUntilClosed(t, ch)
					assertSubscriptionStateReleased(t, b, topic)
					assert.Equal(t, 0, counting.activeFor(topic), "backend released after last subscriber")
					if path.firesLastHook {
						select {
						case <-last:
						case <-time.After(time.Second):
							t.Fatal("OnLastUnsubscribe did not fire")
						}
					}

					// Repeat removal is a no-op.
					unsub()
					unsub()
					b.Close()
					assert.Equal(t, 0, counting.activeFor(topic))
				})
			}
		}
	}
}

// TestLifecycleBackendDeliveryAllVariants checks that every variant receives
// messages published on another instance and that the backend registration
// ends with the subscription.
func TestLifecycleBackendDeliveryAllVariants(t *testing.T) {
	for _, v := range lifecycleVariants() {
		t.Run(v.name, func(t *testing.T) {
			const topic = "orders"
			mem := memory.New()
			defer mem.Close()
			local := newCountingBackend(mem.Fork())
			remote := NewSSEBroker(WithBackend(mem))
			defer remote.Close()
			b := NewSSEBroker(WithBackend(local))
			defer b.Close()

			ch, unsub := v.subscribe(b, topic)
			require.Equal(t, 1, local.activeFor(topic))
			publishFor(remote, v, topic, "remote")
			receiveValue(t, ch, "remote")

			unsub()
			assert.Equal(t, 0, local.activeFor(topic))
		})
	}
}

// TestCoalescingWorkerStopsOnClose covers a coalescing subscriber whose
// output is full and unread when the broker closes.
func TestCoalescingWorkerStopsOnClose(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			b := NewSSEBroker()
			var ch <-chan string
			var unsub func()
			publish := b.Publish
			if scoped {
				ch, unsub = b.SubscribeScopedWithCoalescing("ticker", "s")
				publish = func(topic, msg string) { b.PublishTo(topic, "s", msg) }
			} else {
				ch, unsub = b.SubscribeWithCoalescing("ticker")
			}

			// The first value fills the output buffer; the worker then blocks
			// trying to deliver the second.
			publish("ticker", "v1")
			require.Eventually(t, func() bool { return len(ch) == 1 }, time.Second, time.Millisecond)
			publish("ticker", "v2")

			b.Close()
			drainUntilClosed(t, ch)
			unsub()
			unsub()
		})
	}
}

// TestCoalescingUnsubscribeWithFullOutput checks the worker also exits on
// unsubscribe while blocked on a full, unread output.
func TestCoalescingUnsubscribeWithFullOutput(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	ch, unsub := b.SubscribeWithCoalescing("ticker")
	b.Publish("ticker", "v1")
	require.Eventually(t, func() bool { return len(ch) == 1 }, time.Second, time.Millisecond)
	b.Publish("ticker", "v2")

	unsub()
	drainUntilClosed(t, ch)
	unsub()
	assertSubscriptionStateReleased(t, b, "ticker")
}

// TestRateFlushCannotSendAfterRemoval holds a rate-limited message, removes
// the subscriber in every supported way, and lets the flush tick run. The
// pending entry must be gone and the flush must not touch the closed channel.
func TestRateFlushCannotSendAfterRemoval(t *testing.T) {
	removals := map[string]func(b *SSEBroker, unsub func()){
		"unsubscribe": func(_ *SSEBroker, unsub func()) { unsub() },
		"Disconnect":  func(b *SSEBroker, _ func()) { b.Disconnect("ticks", "r1") },
		"Close":       func(b *SSEBroker, _ func()) { b.Close() },
	}
	for name, remove := range removals {
		t.Run(name, func(t *testing.T) {
			b := NewSSEBroker()
			defer b.Close()
			ch, unsub := b.SubscribeWith("ticks", SubWithRate(Rate{MinInterval: time.Hour}), SubWithMeta(SubscribeMeta{ID: "r1"}))
			b.Publish("ticks", "first") // delivered immediately
			b.Publish("ticks", "held")  // held for an hour

			b.rateLimit.mu.Lock()
			require.Len(t, b.rateLimit.subs, 1)
			b.rateLimit.mu.Unlock()

			remove(b, unsub)
			assert.Equal(t, []string{"first"}, drainUntilClosed(t, ch))
			b.rateLimit.mu.Lock()
			assert.Empty(t, b.rateLimit.subs, "removal must drop the pending entry")
			b.rateLimit.mu.Unlock()

			// A flush far in the future would deliver "held" if the entry
			// survived; it must find nothing to send.
			b.rateLimit.flush(time.Now().Add(2 * time.Hour))
		})
	}
}

// TestRateFlushRacesRemoval runs rate flushes, publishes, unsubscribes and
// Close concurrently. Under -race this checks flush sends are serialized with
// channel close rather than relying on recover.
func TestRateFlushRacesRemoval(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		b := NewSSEBroker(WithBufferSize(1))
		const subs = 8
		unsubs := make([]func(), subs)
		chans := make([]<-chan string, subs)
		for i := range subs {
			chans[i], unsubs[i] = b.SubscribeWithRate("ticks", Rate{MinInterval: time.Nanosecond})
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				b.Publish("ticks", "m")
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				b.rateLimit.flush(time.Now())
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for i, u := range unsubs {
				u()
				if i == subs/2 {
					b.Close()
				}
			}
		}()
		close(start)
		wg.Wait()
		for _, ch := range chans {
			drainUntilClosed(t, ch)
		}
	}
}

// TestRateRegisteredBeforeVisible checks that the first publish after a
// rate-limited subscribe already sees the limit, even when it races the
// subscribe call.
func TestRateRegisteredBeforeVisible(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(16))
	defer b.Close()
	// Fire publishes from inside OnFirstSubscriber, which runs as soon as
	// the channel is registered.
	published := make(chan struct{})
	b.OnFirstSubscriber("ticks", func(topic string) {
		for i := 0; i < 5; i++ {
			b.Publish(topic, fmt.Sprintf("m%d", i))
		}
		close(published)
	})
	ch, unsub := b.SubscribeWithRate("ticks", Rate{MinInterval: time.Hour})
	defer unsub()
	<-published
	// At most one message may pass an hour-long limit.
	assert.LessOrEqual(t, len(ch), 1)
}

// TestDisconnectReleasesBackendAndState covers administrative Disconnect of
// the last subscriber: the backend subscription and per-subscriber state are
// released and a new subscriber registers the topic again.
func TestDisconnectReleasesBackendAndState(t *testing.T) {
	mem := memory.New()
	defer mem.Close()
	counting := newCountingBackend(mem)
	b := NewSSEBroker(WithBackend(counting), WithAdaptiveBackpressure(AdaptiveBackpressure{ThrottleAt: 1, SimplifyAt: 2, DisconnectAt: 3}))
	defer b.Close()

	ch, _ := b.SubscribeWithMeta("orders", SubscribeMeta{ID: "u1"})
	require.Equal(t, 1, counting.activeFor("orders"))
	require.True(t, b.Disconnect("orders", "u1"))
	drainUntilClosed(t, ch)
	assert.Equal(t, 0, counting.activeFor("orders"))
	assertSubscriptionStateReleased(t, b, "orders")

	_, unsub := b.SubscribeWithMeta("orders", SubscribeMeta{ID: "u2"})
	defer unsub()
	assert.Equal(t, 1, counting.activeFor("orders"))
}

// gatedBackend wraps a counting backend and can hold Unsubscribe until the
// test releases it, exposing first/last transition interleavings.
type gatedBackend struct {
	*countingBackend
	gate    chan struct{} // nil = no gating
	entered chan struct{}
	subs    atomic.Int64
}

func (g *gatedBackend) Subscribe(ctx context.Context, topic string) (<-chan backend.MessageEnvelope, error) {
	g.subs.Add(1)
	return g.countingBackend.Subscribe(ctx, topic)
}

func (g *gatedBackend) Unsubscribe(topic string) error {
	if g.gate != nil {
		g.entered <- struct{}{}
		<-g.gate
	}
	return g.countingBackend.Unsubscribe(topic)
}

// TestBackendTransitionsInterleave holds the last subscriber's backend
// unsubscribe while a new first subscriber joins. The new subscriber must
// end with backend coverage, and the backend must see one subscription per
// active period.
func TestBackendTransitionsInterleave(t *testing.T) {
	mem := memory.New()
	defer mem.Close()
	gated := &gatedBackend{
		countingBackend: newCountingBackend(mem.Fork()),
		gate:            make(chan struct{}),
		entered:         make(chan struct{}),
	}
	remote := NewSSEBroker(WithBackend(mem))
	defer remote.Close()
	b := NewSSEBroker(WithBackend(gated))
	defer b.Close()

	_, unsubA := b.Subscribe("orders")
	unsubDone := make(chan struct{})
	go func() {
		unsubA()
		close(unsubDone)
	}()
	select {
	case <-gated.entered: // A's backend unsubscribe is in progress
	case <-time.After(2 * time.Second):
		t.Fatal("last unsubscribe never reached the backend")
	}

	type result struct {
		ch    <-chan string
		unsub func()
	}
	joined := make(chan result, 1)
	go func() {
		ch, unsub := b.Subscribe("orders")
		joined <- result{ch, unsub}
	}()
	// Wait until B is registered locally, then let A's unsubscribe finish.
	require.Eventually(t, func() bool { return b.HasSubscribers("orders") }, time.Second, time.Millisecond)
	close(gated.gate)
	<-unsubDone
	sub := <-joined
	gated.gate = nil

	assert.Equal(t, 1, gated.activeFor("orders"), "new subscriber keeps backend coverage")
	assert.Equal(t, int64(2), gated.subs.Load(), "one backend subscription per active period")

	remote.Publish("orders", "after")
	receiveValue(t, sub.ch, "after")
	sub.unsub()
	assert.Equal(t, 0, gated.activeFor("orders"))
}

// TestBackendResubscribeNoDuplicateFanIn checks that cycling the only
// subscriber on and off does not leave stale fan-in that duplicates
// cross-instance messages.
func TestBackendResubscribeNoDuplicateFanIn(t *testing.T) {
	mem := memory.New()
	defer mem.Close()
	remote := NewSSEBroker(WithBackend(mem))
	defer remote.Close()
	b := NewSSEBroker(WithBackend(mem.Fork()), WithBufferSize(16))
	defer b.Close()

	for i := 0; i < 3; i++ {
		_, unsub := b.SubscribeWithFilter("orders", acceptAll)
		unsub()
	}
	ch, unsub := b.SubscribeScoped("orders", "s")
	defer unsub()
	remote.PublishTo("orders", "s", "once")
	receiveValue(t, ch, "once")
	select {
	case extra := <-ch:
		t.Fatalf("duplicate delivery %q", extra)
	case <-time.After(50 * time.Millisecond):
	}

	// Backend reconnect re-registers active topics without adding fan-in.
	b.resubscribeAll()
	remote.PublishTo("orders", "s", "again")
	receiveValue(t, ch, "again")
	select {
	case extra := <-ch:
		t.Fatalf("duplicate delivery after reconnect %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestLifecycleConcurrentStress exercises registration, ordinary and blocking
// publish, rate flush, coalescing updates, unsubscribe and Close together.
// It checks for races and that no worker goroutine outlives the broker.
func TestLifecycleConcurrentStress(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for iter := 0; iter < 10; iter++ {
		mem := memory.New()
		b := NewSSEBroker(WithBufferSize(1), WithSlowSubscriberEviction(2), WithBackend(mem))
		start := make(chan struct{})
		var wg sync.WaitGroup
		variants := lifecycleVariants()
		for i, v := range variants {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 20; j++ {
					ch, unsub := v.subscribe(b, "t")
					if ch == nil {
						return
					}
					if (i+j)%3 == 0 {
						unsub()
					}
					go func() {
						for range ch { //nolint:revive // drain until closed
						}
					}()
				}
			}()
		}
		halfway := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				if j == 100 {
					close(halfway)
				}
				b.Publish("t", "m")
				b.PublishTo("t", "s", "m")
				_ = b.PublishBlocking("t", "b", time.Microsecond)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				b.rateLimit.flush(time.Now())
			}
		}()
		close(start)
		<-halfway
		b.Close()
		wg.Wait()
		mem.Close()
	}
	for i := 0; runtime.NumGoroutine() > baseline+2; i++ {
		if i > 200 {
			t.Fatalf("goroutines leaked: baseline %d, now %d", baseline, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGlobUnsubscribeIdempotent covers repeat and overlapping glob removal.
func TestGlobUnsubscribeIdempotent(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.SubscribeGlob("a/*")
	b.UnsubscribeGlob(ch)
	unsub()
	unsub()

	_, unsubFiltered := b.SubscribeGlobWith("a/*", SubWithFilter(acceptAll))
	unsubFiltered()
	unsubFiltered()

	_, unsubAfterClose := b.SubscribeGlob("b/*")
	b.Close()
	unsubAfterClose()
}
