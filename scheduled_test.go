package tavern

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduledPublisher_BasicPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("stats", WithBaseTick(10*time.Millisecond))
	pub.Register("greeting", 20*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("hello")
		return nil
	})

	ch, unsub := b.Subscribe("stats")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for message")
	}
}

func TestScheduledPublisher_MultipleSections(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("multi", WithBaseTick(10*time.Millisecond))
	pub.Register("alpha", 20*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("[alpha]")
		return nil
	})
	pub.Register("beta", 20*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("[beta]")
		return nil
	})

	ch, unsub := b.Subscribe("multi")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "[alpha]")
		assert.Contains(t, msg, "[beta]")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for message")
	}
}

func TestScheduledPublisher_SkipsWhenNoSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var calls atomic.Int64
	pub := b.NewScheduledPublisher("idle", WithBaseTick(10*time.Millisecond))
	pub.Register("counter", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		calls.Add(1)
		buf.WriteString("tick")
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	// No subscriber — render should not be called.
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, int64(0), calls.Load(), "render should not be called without subscribers")

	// Now subscribe and verify it starts rendering.
	ch, unsub := b.Subscribe("idle")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "tick", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for message after subscribing")
	}
	assert.Greater(t, calls.Load(), int64(0))
}

func TestScheduledPublisher_SetInterval(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("interval", WithBaseTick(5*time.Millisecond))
	// Start with a very long interval so nothing fires.
	pub.Register("slow", 10*time.Second, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("ok")
		return nil
	})

	ch, unsub := b.Subscribe("interval")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	// Should not receive anything with 10s interval.
	select {
	case <-ch:
		t.Fatal("should not receive message with long interval")
	case <-time.After(50 * time.Millisecond):
	}

	// Shorten the interval.
	ok := pub.SetInterval("slow", 10*time.Millisecond)
	require.True(t, ok)

	select {
	case msg := <-ch:
		assert.Equal(t, "ok", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out after shortening interval")
	}
}

func TestScheduledPublisher_SetInterval_NotFound(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("nope")
	ok := pub.SetInterval("nonexistent", time.Second)
	assert.False(t, ok)
}

func TestScheduledPublisher_PanicRecovery(t *testing.T) {
	b := NewSSEBroker(WithLogger(slog.Default()))
	defer b.Close()

	pub := b.NewScheduledPublisher("panic", WithBaseTick(10*time.Millisecond))
	pub.Register("bad", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		panic("boom")
	})
	pub.Register("good", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("still alive")
		return nil
	})

	ch, unsub := b.Subscribe("panic")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "still alive")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out — publisher may have crashed")
	}
}

func TestScheduledPublisher_RenderError(t *testing.T) {
	b := NewSSEBroker(WithLogger(slog.Default()))
	defer b.Close()

	pub := b.NewScheduledPublisher("err", WithBaseTick(10*time.Millisecond))
	pub.Register("failing", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		return errors.New("render failed")
	})
	pub.Register("working", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("ok")
		return nil
	})

	ch, unsub := b.Subscribe("err")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.RunPublisher(ctx, pub.Start)

	select {
	case msg := <-ch:
		assert.Equal(t, "ok", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out — publisher should continue after render error")
	}
}

func TestScheduledPublisher_StopsOnContextCancel(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("cancel", WithBaseTick(10*time.Millisecond))
	pub.Register("tick", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("x")
		return nil
	})

	ch, unsub := b.Subscribe("cancel")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	b.RunPublisher(ctx, pub.Start)

	// Wait for at least one message.
	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for first message")
	}

	cancel()

	// Drain any buffered messages then verify no new ones arrive.
	time.Sleep(30 * time.Millisecond)
	for len(ch) > 0 {
		<-ch
	}

	select {
	case _, ok := <-ch:
		if ok {
			// Might get one more buffered message; drain and check again.
			time.Sleep(50 * time.Millisecond)
			select {
			case <-ch:
				t.Fatal("publisher should have stopped after context cancel")
			default:
			}
		}
	case <-time.After(100 * time.Millisecond):
		// No message — publisher stopped. Good.
	}
}

func TestScheduledPublisher_WithBaseTick(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("tick", WithBaseTick(50*time.Millisecond))
	assert.Equal(t, 50*time.Millisecond, pub.baseTick)

	pubDefault := b.NewScheduledPublisher("default")
	assert.Equal(t, 100*time.Millisecond, pubDefault.baseTick)

	// Verify the custom tick actually affects timing: count messages over 200ms.
	var count atomic.Int64
	pub.Register("counter", 50*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		count.Add(1)
		fmt.Fprint(buf, "x")
		return nil
	})

	ch, unsub := b.Subscribe("tick")

	ctx, cancel := context.WithCancel(context.Background())
	b.RunPublisher(ctx, pub.Start)

	// Collect messages for 250ms.
	deadline := time.After(250 * time.Millisecond)
	msgs := 0
loop:
	for {
		select {
		case <-ch:
			msgs++
		case <-deadline:
			break loop
		}
	}

	// Cancel publisher before unsubscribing to avoid send-on-closed-channel race.
	cancel()
	time.Sleep(20 * time.Millisecond)
	unsub()

	// With 50ms base tick and 50ms interval, expect roughly 4-5 messages in 250ms.
	assert.GreaterOrEqual(t, msgs, 2, "expected at least 2 messages with 50ms tick over 250ms")
}
