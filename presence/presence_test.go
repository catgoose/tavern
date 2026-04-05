package presence

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBroker(t *testing.T) *tavern.SSEBroker {
	t.Helper()
	b := tavern.NewSSEBroker()
	t.Cleanup(b.Close)
	return b
}

func TestJoinAndList(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice", Name: "Alice"})
	tr.Join("room", Info{UserID: "bob", Name: "Bob"})

	users := tr.List("room")
	require.Len(t, users, 2)

	ids := map[string]bool{}
	for _, u := range users {
		ids[u.UserID] = true
		assert.False(t, u.JoinedAt.IsZero())
		assert.False(t, u.LastSeen.IsZero())
	}
	assert.True(t, ids["alice"])
	assert.True(t, ids["bob"])
}

func TestJoinDuplicateIsNoop(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice", Name: "Alice"})
	tr.Join("room", Info{UserID: "alice", Name: "Alice2"})

	users := tr.List("room")
	require.Len(t, users, 1)
	assert.Equal(t, "Alice", users[0].Name)
}

func TestLeave(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	tr.Join("room", Info{UserID: "bob"})
	tr.Leave("room", "alice")

	users := tr.List("room")
	require.Len(t, users, 1)
	assert.Equal(t, "bob", users[0].UserID)
}

func TestLeaveNonexistent(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	// Should not panic.
	tr.Leave("room", "ghost")
	tr.Join("room", Info{UserID: "alice"})
	tr.Leave("room", "ghost")

	users := tr.List("room")
	require.Len(t, users, 1)
}

func TestGet(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice", Name: "Alice", Metadata: map[string]any{"role": "admin"}})

	info, ok := tr.Get("room", "alice")
	require.True(t, ok)
	assert.Equal(t, "Alice", info.Name)
	assert.Equal(t, "admin", info.Metadata["role"])

	_, ok = tr.Get("room", "bob")
	assert.False(t, ok)

	_, ok = tr.Get("other", "alice")
	assert.False(t, ok)
}

func TestUpdate(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{
		UserID:   "alice",
		Name:     "Alice",
		Metadata: map[string]any{"cursor": "line:1", "status": "active"},
	})

	// Partial update: change cursor, leave status alone.
	tr.Update("room", "alice", map[string]any{"cursor": "line:42"})

	info, ok := tr.Get("room", "alice")
	require.True(t, ok)
	assert.Equal(t, "line:42", info.Metadata["cursor"])
	assert.Equal(t, "active", info.Metadata["status"])
	assert.Equal(t, "Alice", info.Name, "name should not change on Update")
}

func TestUpdateNonexistent(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	// Should not panic.
	tr.Update("room", "ghost", map[string]any{"x": 1})
	tr.Join("room", Info{UserID: "alice"})
	tr.Update("room", "ghost", map[string]any{"x": 1})
}

func TestUpdateAddsMetadataIfNil(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	tr.Update("room", "alice", map[string]any{"cursor": "line:1"})

	info, ok := tr.Get("room", "alice")
	require.True(t, ok)
	assert.Equal(t, "line:1", info.Metadata["cursor"])
}

func TestHeartbeat(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	before, _ := tr.Get("room", "alice")

	time.Sleep(5 * time.Millisecond)
	tr.Heartbeat("room", "alice")

	after, _ := tr.Get("room", "alice")
	assert.True(t, after.LastSeen.After(before.LastSeen))
}

func TestHeartbeatNonexistent(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	// Should not panic.
	tr.Heartbeat("room", "ghost")
}

func TestStaleDetection(t *testing.T) {
	b := newBroker(t)

	var mu sync.Mutex
	var leaves []string

	tr := New(b, Config{
		StaleTimeout: 50 * time.Millisecond,
		OnLeave: func(_ string, info Info) {
			mu.Lock()
			leaves = append(leaves, info.UserID)
			mu.Unlock()
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	tr.Join("room", Info{UserID: "bob"})

	// Keep bob alive.
	go func() {
		for range 10 {
			time.Sleep(10 * time.Millisecond)
			tr.Heartbeat("room", "bob")
		}
	}()

	// Wait for sweep to fire (interval = StaleTimeout/2 = 25ms).
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	assert.Contains(t, leaves, "alice")
	assert.NotContains(t, leaves, "bob")
	mu.Unlock()

	users := tr.List("room")
	require.Len(t, users, 1)
	assert.Equal(t, "bob", users[0].UserID)
}

func TestCallbacks(t *testing.T) {
	b := newBroker(t)

	var joins, leaves, updates []string
	var mu sync.Mutex

	tr := New(b, Config{
		StaleTimeout: time.Minute,
		OnJoin: func(_ string, info Info) {
			mu.Lock()
			joins = append(joins, info.UserID)
			mu.Unlock()
		},
		OnLeave: func(_ string, info Info) {
			mu.Lock()
			leaves = append(leaves, info.UserID)
			mu.Unlock()
		},
		OnUpdate: func(_ string, info Info) {
			mu.Lock()
			updates = append(updates, info.UserID)
			mu.Unlock()
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	tr.Update("room", "alice", map[string]any{"x": 1})
	tr.Leave("room", "alice")

	mu.Lock()
	assert.Equal(t, []string{"alice"}, joins)
	assert.Equal(t, []string{"alice"}, leaves)
	assert.Equal(t, []string{"alice"}, updates)
	mu.Unlock()
}

func TestOOBPublishing(t *testing.T) {
	b := newBroker(t)

	// Subscribe to the presence OOB topic to capture publishes.
	ch, unsub := b.Subscribe("room:presence")
	defer unsub()

	tr := New(b, Config{
		StaleTimeout: time.Minute,
		RenderFunc: func(_ string, users []Info) string {
			return fmt.Sprintf("<ul>%d users</ul>", len(users))
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "1 users")
		assert.Contains(t, msg, "presence-list")
		assert.Contains(t, msg, "hx-swap-oob")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}

	tr.Join("room", Info{UserID: "bob"})

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "2 users")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}

	tr.Leave("room", "alice")

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "1 users")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestOOBNotPublishedWithoutRenderFunc(t *testing.T) {
	b := newBroker(t)

	ch, unsub := b.Subscribe("room:presence")
	defer unsub()

	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	tr.Leave("room", "alice")

	select {
	case <-ch:
		t.Fatal("should not receive OOB message without RenderFunc")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}
}

func TestCustomTopicSuffixAndTargetID(t *testing.T) {
	b := newBroker(t)

	ch, unsub := b.Subscribe("room.presence")
	defer unsub()

	tr := New(b, Config{
		StaleTimeout:        time.Minute,
		PresenceTopicSuffix: ".presence",
		TargetID:            "user-list",
		RenderFunc: func(_ string, _ []Info) string {
			return "<div>users</div>"
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "user-list")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestOOBOnUpdate(t *testing.T) {
	b := newBroker(t)

	ch, unsub := b.Subscribe("room:presence")
	defer unsub()

	tr := New(b, Config{
		StaleTimeout: time.Minute,
		RenderFunc: func(_ string, users []Info) string {
			for _, u := range users {
				if u.UserID == "alice" {
					if c, ok := u.Metadata["cursor"]; ok {
						return fmt.Sprintf("<span>%s</span>", c)
					}
				}
			}
			return "<span>no cursor</span>"
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	<-ch // drain join message

	tr.Update("room", "alice", map[string]any{"cursor": "line:99"})

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "line:99")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for update OOB")
	}
}

func TestCloseIdempotent(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})

	// Multiple closes should not panic.
	tr.Close()
	tr.Close()
	tr.Close()
}

func TestCloseStopsSweep(t *testing.T) {
	b := newBroker(t)

	var leaveCount atomic.Int64

	tr := New(b, Config{
		StaleTimeout: 20 * time.Millisecond,
		OnLeave: func(_ string, _ Info) {
			leaveCount.Add(1)
		},
	})

	tr.Join("room", Info{UserID: "alice"})
	tr.Close()

	// Wait longer than sweep interval.
	time.Sleep(50 * time.Millisecond)

	// The sweep may or may not have fired before Close — but after Close no
	// new sweep should start, so the count should be stable.
	count := leaveCount.Load()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, count, leaveCount.Load(), "no new sweeps after Close")
}

func TestConcurrentAccess(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	const goroutines = 20
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			uid := fmt.Sprintf("user-%d", id)
			tr.Join("room", Info{UserID: uid, Name: uid})
			tr.Heartbeat("room", uid)
			tr.Update("room", uid, map[string]any{"n": id})
			tr.List("room")
			tr.Get("room", uid)
			tr.Leave("room", uid)
		}(i)
	}
	wg.Wait()

	// All users should have left.
	assert.Empty(t, tr.List("room"))
}

func TestMetadataCopyOnJoin(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	meta := map[string]any{"key": "original"}
	tr.Join("room", Info{UserID: "alice", Metadata: meta})

	// Mutate the caller's map — should not affect internal state.
	meta["key"] = "mutated"

	info, ok := tr.Get("room", "alice")
	require.True(t, ok)
	assert.Equal(t, "original", info.Metadata["key"])
}

func TestMetadataCopyOnGet(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice", Metadata: map[string]any{"key": "value"}})

	info, _ := tr.Get("room", "alice")
	info.Metadata["key"] = "changed"

	info2, _ := tr.Get("room", "alice")
	assert.Equal(t, "value", info2.Metadata["key"])
}

func TestListEmptyTopic(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{StaleTimeout: time.Minute})
	defer tr.Close()

	assert.Empty(t, tr.List("nonexistent"))
}

func TestDefaultConfig(t *testing.T) {
	b := newBroker(t)
	tr := New(b, Config{})
	defer tr.Close()

	assert.Equal(t, DefaultStaleTimeout, tr.cfg.StaleTimeout)
	assert.Equal(t, DefaultPresenceTopicSuffix, tr.cfg.PresenceTopicSuffix)
	assert.Equal(t, DefaultTargetID, tr.cfg.TargetID)
}

func TestStaleDetectionPublishesOOB(t *testing.T) {
	b := newBroker(t)

	ch, unsub := b.Subscribe("room:presence")
	defer unsub()

	tr := New(b, Config{
		StaleTimeout: 30 * time.Millisecond,
		RenderFunc: func(_ string, users []Info) string {
			return fmt.Sprintf("<ul>%d</ul>", len(users))
		},
	})
	defer tr.Close()

	tr.Join("room", Info{UserID: "alice"})
	<-ch // drain join OOB

	// Wait for stale removal.
	time.Sleep(80 * time.Millisecond)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "<ul>0</ul>")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale OOB")
	}
}
