package tavern

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// MemoryReplayStore unit tests
// ---------------------------------------------------------------------------

func TestMemoryReplayStore_AppendAndLatest(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 5))
	for i := 1; i <= 3; i++ {
		require.NoError(t, s.Append(ctx, "t", ReplayEntry{
			ID:  fmt.Sprintf("%d", i),
			Msg: fmt.Sprintf("msg-%d", i),
		}))
	}

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "msg-1", entries[0].Msg)
	assert.Equal(t, "msg-3", entries[2].Msg)
}

func TestMemoryReplayStore_LatestRespectsLimit(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	for i := 1; i <= 5; i++ {
		require.NoError(t, s.Append(ctx, "t", ReplayEntry{
			ID:  fmt.Sprintf("%d", i),
			Msg: fmt.Sprintf("msg-%d", i),
		}))
	}

	entries, err := s.Latest(ctx, "t", 2)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "msg-4", entries[0].Msg)
	assert.Equal(t, "msg-5", entries[1].Msg)
}

func TestMemoryReplayStore_AfterID_Found(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	for i := 1; i <= 5; i++ {
		require.NoError(t, s.Append(ctx, "t", ReplayEntry{
			ID:  fmt.Sprintf("%d", i),
			Msg: fmt.Sprintf("msg-%d", i),
		}))
	}

	entries, found, err := s.AfterID(ctx, "t", "3", 0)
	require.NoError(t, err)
	assert.True(t, found)
	require.Len(t, entries, 2)
	assert.Equal(t, "msg-4", entries[0].Msg)
	assert.Equal(t, "msg-5", entries[1].Msg)
}

func TestMemoryReplayStore_AfterID_NotFound(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{ID: "1", Msg: "m1"}))

	entries, found, err := s.AfterID(ctx, "t", "unknown", 0)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, entries)
}

func TestMemoryReplayStore_AfterID_AllCaughtUp(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{ID: "1", Msg: "m1"}))

	entries, found, err := s.AfterID(ctx, "t", "1", 0)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Empty(t, entries)
}

func TestMemoryReplayStore_TTLFiltering(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	// One expired entry, one live entry.
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{
		ID:        "1",
		Msg:       "expired",
		ExpiresAt: time.Now().Add(-time.Second),
	}))
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{
		ID:  "2",
		Msg: "live",
	}))

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "live", entries[0].Msg)

	// AfterID should also filter expired.
	entries, found, err := s.AfterID(ctx, "t", "1", 0)
	require.NoError(t, err)
	assert.True(t, found)
	require.Len(t, entries, 1)
	assert.Equal(t, "live", entries[0].Msg)
}

func TestMemoryReplayStore_DeleteTopic(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{ID: "1", Msg: "m1"}))

	require.NoError(t, s.DeleteTopic(ctx, "t"))

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMemoryReplayStore_MaxSizeTruncation(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 3))
	for i := 1; i <= 5; i++ {
		require.NoError(t, s.Append(ctx, "t", ReplayEntry{
			ID:  fmt.Sprintf("%d", i),
			Msg: fmt.Sprintf("msg-%d", i),
		}))
	}

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "msg-3", entries[0].Msg)
	assert.Equal(t, "msg-5", entries[2].Msg)
}

func TestMemoryReplayStore_SetMaxEntries_Shrinks(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	require.NoError(t, s.SetMaxEntries(ctx, "t", 10))
	for i := 1; i <= 5; i++ {
		require.NoError(t, s.Append(ctx, "t", ReplayEntry{
			ID:  fmt.Sprintf("%d", i),
			Msg: fmt.Sprintf("msg-%d", i),
		}))
	}
	// Shrink to 2.
	require.NoError(t, s.SetMaxEntries(ctx, "t", 2))

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "msg-4", entries[0].Msg)
	assert.Equal(t, "msg-5", entries[1].Msg)
}

func TestMemoryReplayStore_DefaultMaxSizeIsOne(t *testing.T) {
	s := NewMemoryReplayStore()
	ctx := context.Background()

	// Don't call SetMaxEntries — default should be 1.
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{ID: "1", Msg: "first"}))
	require.NoError(t, s.Append(ctx, "t", ReplayEntry{ID: "2", Msg: "second"}))

	entries, err := s.Latest(ctx, "t", 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "second", entries[0].Msg)
}

// ---------------------------------------------------------------------------
// Broker + ReplayStore integration tests
// ---------------------------------------------------------------------------

func TestBrokerWithReplayStore_PublishWithID_SubscribeFromID(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 5; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	ch, unsub := b.SubscribeFromID("events", "3")
	defer unsub()

	// Drain reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	var got []string
	for range 2 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "msg-4")
	assert.Contains(t, got[0], "id: 4")
	assert.Contains(t, got[1], "msg-5")
	assert.Contains(t, got[1], "id: 5")
}

func TestBrokerWithReplayStore_GapDetection(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 3)
	// Use GapFallbackToSnapshot so we get a control event to verify gap detection.
	b.SetReplayGapPolicy("events", GapFallbackToSnapshot, nil)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Subscribe with an ID that doesn't exist (gap).
	ch, unsub := b.SubscribeFromID("events", "unknown-id")
	defer unsub()

	// Reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// Gap control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-replay-gap")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gap control event")
	}

	// No replay messages.
	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerWithReplayStore_SubscribeReplay(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 5)
	b.PublishWithReplay("events", "hello")
	b.PublishWithReplay("events", "world")

	ch, unsub := b.Subscribe("events")
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
	assert.Equal(t, []string{"hello", "world"}, got)
}

func TestBrokerWithReplayStore_SetReplayPolicyZero_DeletesTopic(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithReplay("events", "hello")

	// Verify it's in the store.
	entries, err := store.Latest(context.Background(), "events", 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// Setting policy to 0 should delete from store.
	b.SetReplayPolicy("events", 0)

	entries, err = store.Latest(context.Background(), "events", 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestBrokerWithReplayStore_SubscribeFromID_EmptyID(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Empty ID replays all via Latest().
	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	var got []string
	for range 3 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
	require.Len(t, got, 3)
	for i, msg := range got {
		assert.Contains(t, msg, fmt.Sprintf("msg-%d", i+1))
		assert.Contains(t, msg, fmt.Sprintf("id: %d", i+1))
	}
}

func TestBrokerWithReplayStore_ClearReplay(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithReplay("events", "hello")

	b.ClearReplay("events")

	entries, err := store.Latest(context.Background(), "events", 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestBrokerWithReplayStore_PublishWithTTL(t *testing.T) {
	store := NewMemoryReplayStore()
	b := NewSSEBroker(WithReplayStore(store))
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithTTL("events", "ttl-msg", time.Hour)

	entries, err := store.Latest(context.Background(), "events", 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "ttl-msg", entries[0].Msg)
	assert.False(t, entries[0].ExpiresAt.IsZero())
}
