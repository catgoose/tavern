package memory

import (
	"context"
	"testing"
	"time"

	"github.com/catgoose/tavern/backend"
	"github.com/stretchr/testify/require"
)

func TestPublishReachesFork(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()
	defer b1.Close()
	defer b2.Close()

	ctx := context.Background()
	ch, err := b2.Subscribe(ctx, "orders")
	require.NoError(t, err)

	env := backend.MessageEnvelope{Topic: "orders", Data: "new-order"}
	require.NoError(t, b1.Publish(ctx, env))

	select {
	case got := <-ch:
		require.Equal(t, env, got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishDoesNotEcho(t *testing.T) {
	b1 := New()
	defer b1.Close()

	ctx := context.Background()
	ch, err := b1.Subscribe(ctx, "orders")
	require.NoError(t, err)

	env := backend.MessageEnvelope{Topic: "orders", Data: "self"}
	require.NoError(t, b1.Publish(ctx, env))

	select {
	case <-ch:
		t.Fatal("message should not echo back to sender")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestBidirectional(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()
	defer b1.Close()
	defer b2.Close()

	ctx := context.Background()
	ch1, err := b1.Subscribe(ctx, "chat")
	require.NoError(t, err)
	ch2, err := b2.Subscribe(ctx, "chat")
	require.NoError(t, err)

	// b1 → b2
	require.NoError(t, b1.Publish(ctx, backend.MessageEnvelope{Topic: "chat", Data: "hello"}))
	select {
	case got := <-ch2:
		require.Equal(t, "hello", got.Data)
	case <-time.After(time.Second):
		t.Fatal("b2 did not receive message from b1")
	}

	// b2 → b1
	require.NoError(t, b2.Publish(ctx, backend.MessageEnvelope{Topic: "chat", Data: "world"}))
	select {
	case got := <-ch1:
		require.Equal(t, "world", got.Data)
	case <-time.After(time.Second):
		t.Fatal("b1 did not receive message from b2")
	}
}

func TestUnsubscribe(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()
	defer b1.Close()
	defer b2.Close()

	ctx := context.Background()
	ch, err := b2.Subscribe(ctx, "orders")
	require.NoError(t, err)

	require.NoError(t, b2.Unsubscribe("orders"))

	// Channel should be closed after unsubscribe.
	_, open := <-ch
	require.False(t, open)
}

func TestCloseClosesChannels(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()

	ctx := context.Background()
	ch, err := b2.Subscribe(ctx, "orders")
	require.NoError(t, err)

	require.NoError(t, b2.Close())
	_, open := <-ch
	require.False(t, open)

	// Operations on closed backend return ErrClosed.
	require.ErrorIs(t, b2.Publish(ctx, backend.MessageEnvelope{}), ErrClosed)
	_, err = b2.Subscribe(ctx, "orders")
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, b2.Unsubscribe("orders"), ErrClosed)

	b1.Close()
}

func TestSubscribeIdempotent(t *testing.T) {
	b := New()
	defer b.Close()

	ctx := context.Background()
	ch1, err := b.Subscribe(ctx, "t")
	require.NoError(t, err)
	ch2, err := b.Subscribe(ctx, "t")
	require.NoError(t, err)

	// Same channel returned for duplicate subscribe.
	require.Equal(t, ch1, ch2)
}

func TestScopedEnvelope(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()
	defer b1.Close()
	defer b2.Close()

	ctx := context.Background()
	ch, err := b2.Subscribe(ctx, "orders")
	require.NoError(t, err)

	env := backend.MessageEnvelope{Topic: "orders", Data: "scoped", Scope: "user:42"}
	require.NoError(t, b1.Publish(ctx, env))

	select {
	case got := <-ch:
		require.Equal(t, "user:42", got.Scope)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMultipleForks(t *testing.T) {
	b1 := New()
	b2 := b1.Fork()
	b3 := b1.Fork()
	defer b1.Close()
	defer b2.Close()
	defer b3.Close()

	ctx := context.Background()
	ch2, err := b2.Subscribe(ctx, "x")
	require.NoError(t, err)
	ch3, err := b3.Subscribe(ctx, "x")
	require.NoError(t, err)

	require.NoError(t, b1.Publish(ctx, backend.MessageEnvelope{Topic: "x", Data: "fan"}))

	for _, ch := range []<-chan backend.MessageEnvelope{ch2, ch3} {
		select {
		case got := <-ch:
			require.Equal(t, "fan", got.Data)
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
}
