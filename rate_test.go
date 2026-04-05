package tavern

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRate_Interval(t *testing.T) {
	t.Run("MinInterval takes precedence", func(t *testing.T) {
		r := Rate{MaxPerSecond: 10, MinInterval: 200 * time.Millisecond}
		assert.Equal(t, 200*time.Millisecond, r.interval())
	})

	t.Run("MaxPerSecond converts", func(t *testing.T) {
		r := Rate{MaxPerSecond: 2}
		assert.Equal(t, 500*time.Millisecond, r.interval())
	})

	t.Run("zero rate returns zero", func(t *testing.T) {
		r := Rate{}
		assert.Equal(t, time.Duration(0), r.interval())
	})
}

func TestSubscribeWithRate_BasicRateLimiting(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithRate("metrics", Rate{MinInterval: 100 * time.Millisecond})
	defer unsub()

	// First message should be immediate (no previous send).
	b.Publish("metrics", "msg-1")
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-1", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on first message")
	}

	// Rapid publishes within the interval — should be held.
	b.Publish("metrics", "msg-2")
	b.Publish("metrics", "msg-3")

	// Nothing should arrive immediately.
	select {
	case <-ch:
		t.Fatal("message should be held during rate limit interval")
	case <-time.After(50 * time.Millisecond):
	}

	// After the interval the latest held message should arrive.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-3", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rate-limited message")
	}
}

func TestSubscribeWithRate_LatestWins(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithRate("topic", Rate{MinInterval: 100 * time.Millisecond})
	defer unsub()

	// Consume first message immediately.
	b.Publish("topic", "first")
	<-ch

	// Rapid publishes — only the latest should be delivered.
	b.Publish("topic", "second")
	b.Publish("topic", "third")
	b.Publish("topic", "fourth")

	select {
	case msg := <-ch:
		assert.Equal(t, "fourth", msg, "latest held message should win")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// No extra messages.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSubscribeWithRate_RateRecovery(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithRate("topic", Rate{MinInterval: 50 * time.Millisecond})
	defer unsub()

	// First message immediate.
	b.Publish("topic", "msg-1")
	<-ch

	// Wait for interval to pass.
	time.Sleep(60 * time.Millisecond)

	// Next message should also be immediate.
	b.Publish("topic", "msg-2")
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("message after rate recovery should be immediate")
	}
}

func TestSubscribeWithRate_DoesNotAffectOtherSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// One rate-limited subscriber, one normal subscriber.
	rateCh, rateUnsub := b.SubscribeWithRate("topic", Rate{MinInterval: 200 * time.Millisecond})
	defer rateUnsub()

	normalCh, normalUnsub := b.Subscribe("topic")
	defer normalUnsub()

	// First message goes to both.
	b.Publish("topic", "msg-1")
	<-rateCh
	<-normalCh

	// Rapid publish — normal subscriber gets it immediately, rate-limited is held.
	b.Publish("topic", "msg-2")

	select {
	case msg := <-normalCh:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("normal subscriber should receive immediately")
	}

	// Rate-limited subscriber should not have it yet.
	select {
	case <-rateCh:
		t.Fatal("rate-limited subscriber should not receive within interval")
	case <-time.After(30 * time.Millisecond):
	}

	// Eventually the rate-limited subscriber gets the message.
	select {
	case msg := <-rateCh:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("rate-limited subscriber should eventually receive")
	}
}

func TestSubscribeWithRate_DoesNotCountAsDropped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeWithRate("topic", Rate{MinInterval: 100 * time.Millisecond})
	defer unsub()

	b.Publish("topic", "msg-1")
	<-ch

	// Rapid publishes — held, not dropped.
	b.Publish("topic", "msg-2")
	b.Publish("topic", "msg-3")

	assert.Equal(t, int64(0), b.PublishDrops(), "rate-limited messages should not count as drops")
}

func TestSubscribeScopedWithRate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScopedWithRate("metrics", "user:123", Rate{MinInterval: 100 * time.Millisecond})
	defer unsub()

	// Scoped publish — first is immediate.
	b.PublishTo("metrics", "user:123", "data-1")
	select {
	case msg := <-ch:
		assert.Equal(t, "data-1", msg)
	case <-time.After(time.Second):
		t.Fatal("first scoped message should be immediate")
	}

	// Rapid scoped publishes.
	b.PublishTo("metrics", "user:123", "data-2")
	b.PublishTo("metrics", "user:123", "data-3")

	select {
	case <-ch:
		t.Fatal("should be held during interval")
	case <-time.After(30 * time.Millisecond):
	}

	select {
	case msg := <-ch:
		assert.Equal(t, "data-3", msg)
	case <-time.After(time.Second):
		t.Fatal("latest scoped message should arrive after interval")
	}
}

func TestSubscribeWithRate_SharedTickerCleanup(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub1 := b.SubscribeWithRate("t1", Rate{MinInterval: 50 * time.Millisecond})
	_, unsub2 := b.SubscribeWithRate("t2", Rate{MinInterval: 50 * time.Millisecond})

	require.True(t, b.rateLimit.running, "ticker should be running with subscribers")

	unsub1()
	require.True(t, b.rateLimit.running, "ticker should still run with one subscriber")

	unsub2()
	// After both unsubscribe, ticker should stop.
	b.rateLimit.mu.Lock()
	running := b.rateLimit.running
	subsLen := len(b.rateLimit.subs)
	b.rateLimit.mu.Unlock()
	assert.False(t, running, "ticker should stop when all rate-limited subscribers leave")
	assert.Equal(t, 0, subsLen)
}

func TestSubscribeWithRate_CloseStopsTicker(t *testing.T) {
	b := NewSSEBroker()

	ch, _ := b.SubscribeWithRate("topic", Rate{MinInterval: 5 * time.Second})

	// First message immediate.
	b.Publish("topic", "first")
	<-ch

	// Hold a message.
	b.Publish("topic", "should-not-arrive")

	b.Close()

	// Verify nothing arrives (channel is closed).
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("unexpected message after close: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSubscribeWithRate_MaxPerSecond(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// 2 per second = 500ms interval.
	ch, unsub := b.SubscribeWithRate("topic", Rate{MaxPerSecond: 2})
	defer unsub()

	b.Publish("topic", "msg-1")
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-1", msg)
	case <-time.After(time.Second):
		t.Fatal("first message should be immediate")
	}

	// Publish within interval — should be held.
	b.Publish("topic", "msg-2")
	select {
	case <-ch:
		t.Fatal("should be rate-limited at 2/sec")
	case <-time.After(100 * time.Millisecond):
	}

	// Eventually arrives.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("held message should arrive after interval")
	}
}

func TestSubscribeWithRate_ZeroRateNoOp(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Zero rate should behave like normal Subscribe.
	ch, unsub := b.SubscribeWithRate("topic", Rate{})
	defer unsub()

	b.Publish("topic", "msg-1")
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-1", msg)
	case <-time.After(time.Second):
		t.Fatal("zero rate should not limit")
	}

	b.Publish("topic", "msg-2")
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("zero rate should not limit")
	}
}
