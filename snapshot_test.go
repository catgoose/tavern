package tavern

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSubscribeWithSnapshot_ReceivesSnapshot(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithSnapshot("t", func() string {
		return "initial-state"
	})
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "initial-state", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for snapshot")
	}
}

func TestSubscribeWithSnapshot_ThenLiveMessages(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithSnapshot("t", func() string {
		return "snapshot"
	})
	defer unsub()

	// Drain snapshot
	<-ch

	// Live message
	b.Publish("t", "live")

	select {
	case msg := <-ch:
		assert.Equal(t, "live", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSubscribeWithSnapshot_EmptySnapshotSkipped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithSnapshot("t", func() string {
		return ""
	})
	defer unsub()

	b.Publish("t", "live")

	select {
	case msg := <-ch:
		assert.Equal(t, "live", msg) // first message is live, not snapshot
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestSubscribeWithSnapshot_LifecycleHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnFirstSubscriber("t", func(topic string) { fired <- topic })

	_, unsub := b.SubscribeWithSnapshot("t", func() string { return "snap" })
	defer unsub()

	select {
	case got := <-fired:
		assert.Equal(t, "t", got)
	case <-time.After(time.Second):
		t.Fatal("hook did not fire")
	}
}

func TestSubscribeWithSnapshot_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.SubscribeWithSnapshot("t", func() string { return "snap" })
	defer unsub()

	_, ok := <-ch
	assert.False(t, ok)
}

func TestSubscribeWithSnapshot_CountsSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeWithSnapshot("t", func() string { return "" })
	defer unsub()

	assert.True(t, b.HasSubscribers("t"))
	assert.Equal(t, 1, b.SubscriberCount())
}
