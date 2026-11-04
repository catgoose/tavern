package tavern

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const scopedTTL = 150 * time.Millisecond

func recvWithin(ch <-chan string, d time.Duration) (string, bool) {
	select {
	case m, ok := <-ch:
		return m, ok
	case <-time.After(d):
		return "", false
	}
}

// drainFor returns every message ch delivers within d.
func drainFor(ch <-chan string, d time.Duration) []string {
	var out []string
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-deadline:
			return out
		}
	}
}

func mustNone(t *testing.T, name string, ch <-chan string) {
	t.Helper()
	if got := drainFor(ch, 50*time.Millisecond); len(got) > 0 {
		t.Fatalf("%s received %q", name, got)
	}
}

func scopedBroker(t *testing.T, opts ...BrokerOption) *SSEBroker {
	t.Helper()
	b := NewSSEBroker(append([]BrokerOption{WithMessageTTLSweep(20 * time.Millisecond)}, opts...)...)
	t.Cleanup(b.Close)
	b.SetReplayPolicy("x", 5)
	return b
}

// TestScopedTTLReplayStaysWithItsScope is the reproduction from #245: a
// message for red must not be replayed to blue or to unscoped subscribers,
// and its removal must reach red.
func TestScopedTTLReplayStaysWithItsScope(t *testing.T) {
	b := scopedBroker(t)
	red, unred := b.SubscribeScoped("x", "red")
	defer unred()
	b.PublishToWithTTL("x", "red", "secret-for-red", scopedTTL, WithAutoRemove("n1"))
	if m, _ := recvWithin(red, time.Second); m != "secret-for-red" {
		t.Fatalf("red live: %q", m)
	}

	blue, unblue := b.SubscribeScoped("x", "blue")
	defer unblue()
	plain, unplain := b.Subscribe("x")
	defer unplain()
	lateRed, unLateRed := b.SubscribeScoped("x", "red")
	defer unLateRed()
	if m, _ := recvWithin(lateRed, time.Second); m != "secret-for-red" {
		t.Fatalf("a red subscriber joining before expiry should be replayed the message, got %q", m)
	}
	mustNone(t, "blue (late)", blue)
	mustNone(t, "plain (late)", plain)

	del := `<div id="n1" hx-swap-oob="delete"></div>`
	for name, ch := range map[string]<-chan string{"red": red, "late red": lateRed} {
		if m, _ := recvWithin(ch, scopedTTL+time.Second); m != del {
			t.Fatalf("%s should receive the removal, got %q", name, m)
		}
	}
	mustNone(t, "blue (expiry)", blue)
	mustNone(t, "plain (expiry)", plain)

	after, unAfter := b.SubscribeScoped("x", "red")
	defer unAfter()
	mustNone(t, "red after expiry", after)
}

func TestUnscopedTTLReplayAndRemovalUnchanged(t *testing.T) {
	b := scopedBroker(t)
	plain, unplain := b.Subscribe("x")
	defer unplain()
	b.PublishWithTTL("x", "for everyone", scopedTTL, WithAutoRemove("n2"))
	if m, _ := recvWithin(plain, time.Second); m != "for everyone" {
		t.Fatalf("plain live: %q", m)
	}
	red, unred := b.SubscribeScoped("x", "red")
	defer unred()
	late, unlate := b.Subscribe("x")
	defer unlate()
	for name, ch := range map[string]<-chan string{"red": red, "late plain": late} {
		if m, _ := recvWithin(ch, time.Second); m != "for everyone" {
			t.Fatalf("%s should be replayed an unscoped entry, got %q", name, m)
		}
	}
	del := `<div id="n2" hx-swap-oob="delete"></div>`
	for name, ch := range map[string]<-chan string{"plain": plain, "late plain": late} {
		if m, _ := recvWithin(ch, scopedTTL+time.Second); m != del {
			t.Fatalf("%s should receive the removal, got %q", name, m)
		}
	}
}

// TestScopedTTLReplayAcrossSubscriptionKinds checks every subscription
// that replays the cache: each sees red's entry only with the red scope.
// Coalescing subscriptions do not replay, so they are not among them.
func TestScopedTTLReplayAcrossSubscriptionKinds(t *testing.T) {
	anything := func(string) bool { return true }
	kinds := map[string]func(b *SSEBroker, scope string) (<-chan string, func()){
		"SubscribeScoped": func(b *SSEBroker, s string) (<-chan string, func()) { return b.SubscribeScoped("x", s) },
		"SubscribeScopedWithFilter": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeScopedWithFilter("x", s, anything)
		},
		"SubscribeScopedWithRate": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeScopedWithRate("x", s, Rate{MinInterval: time.Millisecond})
		},
		"SubscribeWith scope+filter": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeWith("x", SubWithScope(s), SubWithFilter(anything))
		},
		"SubscribeWith scope+meta": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeWith("x", SubWithScope(s), SubWithMeta(SubscribeMeta{ID: "m"}))
		},
		"SubscribeWith scope+loss-aware": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeWith("x", SubWithScope(s), SubWithDisconnectOnLoss())
		},
		"SubscribeFromIDWith fresh": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeFromIDWith("x", "", SubWithScope(s))
		},
		"SubscribeFromIDWith fresh+filter": func(b *SSEBroker, s string) (<-chan string, func()) {
			return b.SubscribeFromIDWith("x", "", SubWithScope(s), SubWithFilter(anything))
		},
		"SubscribeMultiWith": func(b *SSEBroker, s string) (<-chan string, func()) {
			ch, unsub := b.SubscribeMultiWith([]string{"x"}, SubWithScope(s))
			out := make(chan string, 8)
			go func() {
				for m := range ch {
					out <- m.Data
				}
			}()
			return out, unsub
		},
	}
	for name, open := range kinds {
		t.Run(name, func(t *testing.T) {
			b := scopedBroker(t)
			b.PublishToWithTTL("x", "red", "secret-for-red", time.Minute)
			blue, unblue := open(b, "blue")
			defer unblue()
			red, unred := open(b, "red")
			defer unred()
			if m, _ := recvWithin(red, time.Second); m != "secret-for-red" {
				t.Fatalf("red: %q", m)
			}
			mustNone(t, "blue", blue)
		})
	}
	t.Run("unscoped kinds", func(t *testing.T) {
		b := scopedBroker(t)
		b.PublishToWithTTL("x", "red", "secret-for-red", time.Minute)
		for name, open := range map[string]func() (<-chan string, func()){
			"Subscribe":                func() (<-chan string, func()) { return b.Subscribe("x") },
			"SubscribeWithFilter":      func() (<-chan string, func()) { return b.SubscribeWithFilter("x", anything) },
			"SubscribeWithCoalescing":  func() (<-chan string, func()) { return b.SubscribeWithCoalescing("x") },
			"SubscribeFromID fresh":    func() (<-chan string, func()) { return b.SubscribeFromID("x", "") },
			"SubscribeWith loss-aware": func() (<-chan string, func()) { return b.SubscribeWith("x", SubWithDisconnectOnLoss()) },
			"SubscribeWithMeta":        func() (<-chan string, func()) { return b.SubscribeWithMeta("x", SubscribeMeta{ID: "p"}) },
		} {
			ch, unsub := open()
			mustNone(t, name, ch)
			unsub()
		}
	})
}

// TestScopedTTLResumeCountsOnlyVisibleEntries resumes a blue subscriber past
// a red entry: it is neither replayed nor counted as missed.
func TestScopedTTLResumeCountsOnlyVisibleEntries(t *testing.T) {
	b := scopedBroker(t)
	var mu sync.Mutex
	var missed []int
	done := make(chan struct{}, 1)
	b.OnReconnect("x", func(info ReconnectInfo) {
		mu.Lock()
		missed = append(missed, info.MissedCount)
		mu.Unlock()
		done <- struct{}{}
	})
	b.PublishWithID("x", "1", "first")
	b.PublishToWithTTL("x", "red", "secret-for-red", time.Minute)
	b.PublishWithID("x", "2", "second")

	blue, unblue := b.SubscribeFromIDWith("x", "1", SubWithScope("blue"))
	defer unblue()
	got := drainFor(blue, 100*time.Millisecond)
	<-done
	for _, m := range got {
		if strings.Contains(m, "secret") {
			t.Fatalf("blue resume replayed red's entry: %q", got)
		}
	}
	if len(got) == 0 || !strings.Contains(got[0], "second") {
		t.Fatalf("blue resume should replay the unscoped entry after 1, got %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if missed[0] != 1 {
		t.Fatalf("blue missed %d entries, want 1 (red's entry is not blue's)", missed[0])
	}
}

func TestScopedTTLInMemoryReplayStore(t *testing.T) {
	b := scopedBroker(t, WithReplayStore(NewMemoryReplayStore()))
	b.PublishToWithTTL("x", "red", "secret-for-red", time.Minute)
	red, unred := b.SubscribeScoped("x", "red")
	defer unred()
	if m, _ := recvWithin(red, time.Second); m != "secret-for-red" {
		t.Fatalf("red from the store: %q", m)
	}
	blue, unblue := b.SubscribeScoped("x", "blue")
	defer unblue()
	plain, unplain := b.Subscribe("x")
	defer unplain()
	resumed, unresumed := b.SubscribeFromIDWith("x", "", SubWithScope("blue"))
	defer unresumed()
	mustNone(t, "blue", blue)
	mustNone(t, "plain", plain)
	mustNone(t, "blue (SubscribeFromIDWith)", resumed)
}

// plainStore is a ReplayStore that does not declare it keeps scopes.
type plainStore struct{ ReplayStore }

func TestScopedTTLWithStoreThatDropsScope(t *testing.T) {
	var mu sync.Mutex
	var errs []*ReplayStoreError
	b := scopedBroker(t,
		WithReplayStore(plainStore{NewMemoryReplayStore()}),
		WithReplayStoreErrorHandler(func(e *ReplayStoreError) { mu.Lock(); errs = append(errs, e); mu.Unlock() }))
	red, unred := b.SubscribeScoped("x", "red")
	defer unred()
	b.PublishToWithTTL("x", "red", "secret-for-red", time.Minute)
	if m, _ := recvWithin(red, time.Second); m != "secret-for-red" {
		t.Fatalf("red still gets live delivery: %q", m)
	}
	mu.Lock()
	if len(errs) != 1 || errs[0].Op != ReplayOpAppend || !errors.Is(errs[0], ErrReplayScopeUnsupported) {
		t.Fatalf("expected one append error with ErrReplayScopeUnsupported, got %v", errs)
	}
	mu.Unlock()
	blue, unblue := b.SubscribeScoped("x", "blue")
	defer unblue()
	plain, unplain := b.Subscribe("x")
	defer unplain()
	mustNone(t, "blue", blue)
	mustNone(t, "plain", plain)
	b.PublishWithTTL("x", "for everyone", time.Minute)
	later, unlater := b.Subscribe("x")
	defer unlater()
	if m, _ := recvWithin(later, time.Second); m != "for everyone" {
		t.Fatalf("unscoped entries are still stored: %q", m)
	}
	entries, err := b.replayStore.Latest(context.Background(), "x", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the store holds only the unscoped entry: %v %v", entries, err)
	}
}

func TestScopedTTLClearedAndTrimmed(t *testing.T) {
	b := scopedBroker(t)
	b.PublishToWithTTL("x", "red", "secret-1", time.Minute)
	b.ClearReplay("x")
	red, unred := b.SubscribeScoped("x", "red")
	mustNone(t, "red after ClearReplay", red)
	unred()

	b.SetReplayPolicy("x", 2)
	for _, m := range []string{"secret-1", "secret-2", "secret-3"} {
		b.PublishToWithTTL("x", "red", m, time.Minute)
	}
	b.PublishWithTTL("x", "public", time.Minute)
	red, unred = b.SubscribeScoped("x", "red")
	defer unred()
	got := drainFor(red, 100*time.Millisecond)
	if strings.Join(got, ",") != "secret-3,public" {
		t.Fatalf("red replays the last two kept entries, got %q", got)
	}
	blue, unblue := b.SubscribeScoped("x", "blue")
	defer unblue()
	if got := drainFor(blue, 100*time.Millisecond); strings.Join(got, ",") != "public" {
		t.Fatalf("blue replays only the unscoped entry, got %q", got)
	}
}
