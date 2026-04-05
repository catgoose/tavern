package tavern

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishWithTTL_ImmediateDelivery(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.Subscribe("toasts")
	defer unsub()

	b.PublishWithTTL("toasts", "hello", 5*time.Second)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithTTL_ReplayBeforeExpiry(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("toasts", 10)
	b.PublishWithTTL("toasts", "toast-msg", 5*time.Second)

	// Subscribe immediately — should get the replay.
	ch, unsub := b.Subscribe("toasts")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "toast-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay")
	}
}

func TestPublishWithTTL_ExpiresFromReplay(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("toasts", 10)
	b.PublishWithTTL("toasts", "ephemeral", 50*time.Millisecond)

	// Wait for the TTL to expire and the sweeper to run.
	time.Sleep(150 * time.Millisecond)

	// New subscriber should NOT get the expired message.
	ch, unsub := b.Subscribe("toasts")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(100 * time.Millisecond):
		// Good — no replay.
	}
}

func TestPublishWithTTL_NonTTLEntriesSurviveSweep(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("mixed", 10)

	// Publish a TTL message and a regular replay message.
	b.PublishWithTTL("mixed", "ephemeral", 50*time.Millisecond)
	b.PublishWithReplay("mixed", "persistent")

	// Wait for TTL to expire.
	time.Sleep(150 * time.Millisecond)

	ch, unsub := b.Subscribe("mixed")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "persistent", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for persistent replay")
	}

	// Should have no more messages.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishWithTTL_AutoRemove(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.Subscribe("toasts")
	defer unsub()

	b.PublishWithTTL("toasts", "<div id=\"toast-1\">Saved!</div>", 50*time.Millisecond,
		WithAutoRemove("toast-1"),
	)

	// Receive the original message.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "Saved!")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}

	// Wait for expiry — should receive an OOB delete fragment.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, `hx-swap-oob="delete"`)
		assert.Contains(t, msg, `id="toast-1"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for auto-remove message")
	}
}

func TestPublishToWithTTL_ScopedDelivery(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	ch1, unsub1 := b.SubscribeScoped("notif", "user:1")
	defer unsub1()
	ch2, unsub2 := b.SubscribeScoped("notif", "user:2")
	defer unsub2()

	b.PublishToWithTTL("notif", "user:1", "for-user-1", 5*time.Second)

	// user:1 should receive.
	select {
	case msg := <-ch1:
		assert.Equal(t, "for-user-1", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped message")
	}

	// user:2 should NOT receive.
	select {
	case msg := <-ch2:
		t.Fatalf("user:2 should not receive scoped message, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishToWithTTL_ExpiresFromReplay(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("notif", 10)
	b.PublishToWithTTL("notif", "user:1", "ephemeral-scoped", 50*time.Millisecond)

	time.Sleep(150 * time.Millisecond)

	ch, unsub := b.Subscribe("notif")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishOOBWithTTL(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.Subscribe("oob-topic")
	defer unsub()

	b.PublishOOBWithTTL("oob-topic", 5*time.Second, Replace("area", "<p>Updated</p>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, `hx-swap-oob="outerHTML"`)
		assert.Contains(t, msg, "Updated")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestPublishIfChangedWithTTL_Dedup(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.Subscribe("dedup")
	defer unsub()

	ok1 := b.PublishIfChangedWithTTL("dedup", "same", 5*time.Second)
	assert.True(t, ok1, "first publish should succeed")

	ok2 := b.PublishIfChangedWithTTL("dedup", "same", 5*time.Second)
	assert.False(t, ok2, "duplicate should be skipped")

	ok3 := b.PublishIfChangedWithTTL("dedup", "different", 5*time.Second)
	assert.True(t, ok3, "changed content should publish")

	// Drain messages.
	var got []string
	for range 2 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
	assert.Equal(t, []string{"same", "different"}, got)
}

func TestPublishIfChangedWithTTL_ExpiresFromReplay(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("dedup-ttl", 10)
	b.PublishIfChangedWithTTL("dedup-ttl", "temp", 50*time.Millisecond)

	time.Sleep(150 * time.Millisecond)

	ch, unsub := b.Subscribe("dedup-ttl")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishWithTTL_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	require.NotPanics(t, func() {
		b.PublishWithTTL("topic", "msg", time.Second)
	})
}

func TestPublishToWithTTL_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	require.NotPanics(t, func() {
		b.PublishToWithTTL("topic", "scope", "msg", time.Second)
	})
}

func TestPublishIfChangedWithTTL_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	require.NotPanics(t, func() {
		ok := b.PublishIfChangedWithTTL("topic", "msg", time.Second)
		assert.False(t, ok)
	})
}

func TestPublishWithTTL_SubscribeFromID_FiltersExpired(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("events", 10)

	// Publish some entries via PublishWithID, then a TTL entry via replay log.
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	// Manually add a TTL entry to the replay log.
	b.mu.Lock()
	log := b.replayLog["events"]
	log = append(log, ReplayEntry{
		ID:        "3",
		Msg:       "ttl-msg",
		ExpiresAt: time.Now().Add(-time.Second), // already expired
	})
	b.replayLog["events"] = log
	b.mu.Unlock()

	// SubscribeFromID from "1" should skip the expired entry.
	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	var got []string
	for range 1 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
	assert.Equal(t, []string{"msg-2"}, got)

	// No more messages.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishWithTTL_SubscribeFromID_EmptyID_FiltersExpired(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("events", 10)

	// Add entries to the replay log with mixed expiry.
	b.mu.Lock()
	b.replayLog["events"] = []ReplayEntry{
		{ID: "1", Msg: "alive"},
		{ID: "2", Msg: "expired", ExpiresAt: time.Now().Add(-time.Second)},
		{ID: "3", Msg: "also-alive"},
	}
	b.replayCache["events"] = []string{"alive", "expired", "also-alive"}
	b.mu.Unlock()

	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	var got []string
	for range 2 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
	assert.Equal(t, []string{"alive", "also-alive"}, got)
}

func TestPublishWithTTL_MultipleAutoRemove(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(10 * time.Millisecond))
	defer b.Close()

	b.SetReplayPolicy("toasts", 10)

	ch, unsub := b.Subscribe("toasts")
	defer unsub()

	b.PublishWithTTL("toasts", "toast-a", 50*time.Millisecond, WithAutoRemove("el-a"))
	b.PublishWithTTL("toasts", "toast-b", 50*time.Millisecond, WithAutoRemove("el-b"))

	// Drain the two original messages.
	for range 2 {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for original message")
		}
	}

	// Wait for expiry — should get two delete fragments.
	var deleteMsgs []string
	timeout := time.After(time.Second)
	for range 2 {
		select {
		case msg := <-ch:
			deleteMsgs = append(deleteMsgs, msg)
		case <-timeout:
			t.Fatalf("timed out waiting for auto-remove, got %d", len(deleteMsgs))
		}
	}

	combined := strings.Join(deleteMsgs, " ")
	assert.Contains(t, combined, "el-a")
	assert.Contains(t, combined, "el-b")
}

func TestWithAutoRemove(t *testing.T) {
	opt := WithAutoRemove("my-element")
	var cfg ttlConfig
	opt(&cfg)
	assert.Equal(t, "my-element", cfg.autoRemoveID)
}

func TestWithMessageTTLSweep(t *testing.T) {
	b := NewSSEBroker(WithMessageTTLSweep(500 * time.Millisecond))
	defer b.Close()
	assert.Equal(t, 500*time.Millisecond, b.msgTTLSweep)
}
