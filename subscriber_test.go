package tavern

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeWithMeta_StoresInfo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeWithMeta("dash", SubscribeMeta{
		ID:   "sess-123",
		Meta: map[string]string{"user": "alice"},
	})
	defer unsub()

	subs := b.Subscribers("dash")
	require.Len(t, subs, 1)
	assert.Equal(t, "sess-123", subs[0].ID)
	assert.Equal(t, "dash", subs[0].Topic)
	assert.Equal(t, "alice", subs[0].Meta["user"])
	assert.False(t, subs[0].ConnectedAt.IsZero())
}

func TestSubscribers_IncludesRegularSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.Subscribe("t")
	defer unsub()

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)
	assert.Equal(t, "t", subs[0].Topic)
	assert.Empty(t, subs[0].ID)
}

func TestSubscribers_IncludesScopedSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeScoped("t", "user-1")
	defer unsub()

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)
	assert.Equal(t, "user-1", subs[0].Scope)
}

func TestSubscribers_IncludesSubscribeFromID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeFromID("t", "")
	defer unsub()

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)
	assert.Equal(t, "t", subs[0].Topic)
}

func TestSubscribers_MetaCopied(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeWithMeta("t", SubscribeMeta{
		ID:   "s1",
		Meta: map[string]string{"key": "val"},
	})
	defer unsub()

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)
	// Mutating the returned copy should not affect the stored info.
	subs[0].Meta["key"] = "changed"
	subs2 := b.Subscribers("t")
	assert.Equal(t, "val", subs2[0].Meta["key"])
}

func TestDisconnect_ByID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, _ := b.SubscribeWithMeta("t", SubscribeMeta{ID: "sess-1"})

	ok := b.Disconnect("t", "sess-1")
	assert.True(t, ok)

	// Channel should be closed.
	_, open := <-ch
	assert.False(t, open)
	assert.False(t, b.HasSubscribers("t"))
}

func TestDisconnect_NotFound(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.Subscribe("t")
	defer unsub()

	ok := b.Disconnect("t", "nonexistent")
	assert.False(t, ok)
	assert.True(t, b.HasSubscribers("t"))
}

func TestDisconnect_ScopedSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// SubscribeScoped doesn't set an ID, so use SubscribeWithMeta for a scoped
	// subscriber that has an ID we can disconnect by.
	ch, _ := b.SubscribeWithMeta("t", SubscribeMeta{ID: "scoped-1"})

	ok := b.Disconnect("t", "scoped-1")
	assert.True(t, ok)

	_, open := <-ch
	assert.False(t, open)
}

func TestSubscribers_EmptyTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	subs := b.Subscribers("nonexistent")
	assert.Empty(t, subs)
}

func TestConnectionEvents_Subscribe(t *testing.T) {
	b := NewSSEBroker(WithConnectionEvents("_meta"))
	defer b.Close()

	metaCh, metaUnsub := b.Subscribe("_meta")
	defer metaUnsub()

	_, unsub := b.Subscribe("dashboard")
	defer unsub()

	select {
	case msg := <-metaCh:
		assert.Contains(t, msg, `"subscribe"`)
		assert.Contains(t, msg, `"dashboard"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for connection event")
	}
}

func TestConnectionEvents_Unsubscribe(t *testing.T) {
	b := NewSSEBroker(WithConnectionEvents("_meta"))
	defer b.Close()

	metaCh, metaUnsub := b.Subscribe("_meta")
	defer metaUnsub()

	_, unsub := b.Subscribe("dashboard")

	// Drain the subscribe event.
	select {
	case <-metaCh:
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	unsub()

	select {
	case msg := <-metaCh:
		assert.Contains(t, msg, `"unsubscribe"`)
		assert.Contains(t, msg, `"dashboard"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unsubscribe event")
	}
}

func TestConnectionEvents_NoRecursion(t *testing.T) {
	b := NewSSEBroker(WithConnectionEvents("_meta"))
	defer b.Close()

	// Subscribing to _meta itself should NOT generate a recursive event.
	metaCh, unsub := b.Subscribe("_meta")
	defer unsub()

	select {
	case msg := <-metaCh:
		t.Fatalf("should not receive event for meta topic subscription: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// correct
	}
}

func TestConnectionEvents_Disabled(t *testing.T) {
	b := NewSSEBroker() // no WithConnectionEvents
	defer b.Close()

	_, unsub := b.Subscribe("t")
	unsub()

	// Should not panic — events just aren't published.
}

func TestConnectionEvents_Disconnect(t *testing.T) {
	b := NewSSEBroker(WithConnectionEvents("_meta"))
	defer b.Close()

	metaCh, metaUnsub := b.Subscribe("_meta")
	defer metaUnsub()

	_, _ = b.SubscribeWithMeta("dashboard", SubscribeMeta{ID: "s1"})

	// Drain subscribe event.
	select {
	case <-metaCh:
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	b.Disconnect("dashboard", "s1")

	select {
	case msg := <-metaCh:
		assert.Contains(t, msg, `"disconnect"`)
		assert.Contains(t, msg, `"dashboard"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for disconnect event")
	}
}

func TestDisconnect_FiresLastHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnLastUnsubscribe("t", func(topic string) { fired <- topic })

	b.SubscribeWithMeta("t", SubscribeMeta{ID: "x"})
	b.Disconnect("t", "x")

	select {
	case got := <-fired:
		assert.Equal(t, "t", got)
	case <-time.After(time.Second):
		t.Fatal("hook did not fire")
	}
}

func TestSubscribers_MultipleSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub1 := b.SubscribeWithMeta("t", SubscribeMeta{ID: "a"})
	defer unsub1()
	_, unsub2 := b.SubscribeWithMeta("t", SubscribeMeta{ID: "b"})
	defer unsub2()
	_, unsub3 := b.SubscribeScoped("t", "scope-1")
	defer unsub3()

	subs := b.Subscribers("t")
	assert.Len(t, subs, 3)

	ids := map[string]bool{}
	for _, s := range subs {
		if s.ID != "" {
			ids[s.ID] = true
		}
	}
	assert.True(t, ids["a"])
	assert.True(t, ids["b"])
}

func TestSubscribers_UnsubscribeRemovesInfo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeWithMeta("t", SubscribeMeta{ID: "s1"})

	subs := b.Subscribers("t")
	require.Len(t, subs, 1)

	unsub()

	subs = b.Subscribers("t")
	assert.Empty(t, subs)
}
