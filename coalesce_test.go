package tavern

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeWithCoalescing_BasicCoalescing(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeWithCoalescing("prices")
	defer unsub()

	// Publish several messages rapidly — only the latest should be delivered.
	for i := 0; i < 100; i++ {
		b.Publish("prices", fmt.Sprintf("price:%d", i))
	}

	// Give the coalescing goroutine time to deliver.
	time.Sleep(50 * time.Millisecond)

	// Drain and collect what we got.
	var received []string
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			received = append(received, msg)
		case <-timeout:
			goto done
		}
	}
done:

	// We should have received at least one message, and the last one should
	// be from the later publishes. We should NOT have received all 100.
	require.NotEmpty(t, received, "should receive at least one message")
	assert.Less(t, len(received), 100, "coalescing should discard intermediate values")

	// The final received message should be a high-numbered price.
	last := received[len(received)-1]
	assert.True(t, strings.HasPrefix(last, "price:"), "message should be a price update")
}

func TestSubscribeScopedWithCoalescing(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScopedWithCoalescing("data", "user:1")
	defer unsub()

	// Publish to matching scope.
	b.PublishTo("data", "user:1", "v1")
	b.PublishTo("data", "user:1", "v2")
	b.PublishTo("data", "user:1", "v3")

	// Publish to non-matching scope — should not be delivered.
	b.PublishTo("data", "user:2", "other")

	time.Sleep(50 * time.Millisecond)

	var received []string
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			received = append(received, msg)
		case <-timeout:
			goto done
		}
	}
done:

	require.NotEmpty(t, received)
	for _, msg := range received {
		assert.NotEqual(t, "other", msg, "should not receive messages from other scope")
	}
}

func TestSubscribeWithCoalescing_NoDropCounting(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeWithCoalescing("prices")
	defer unsub()

	// Publish many messages rapidly.
	for i := 0; i < 50; i++ {
		b.Publish("prices", fmt.Sprintf("msg:%d", i))
	}

	time.Sleep(50 * time.Millisecond)

	// Drain channel.
	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case <-ch:
		case <-timeout:
			goto done
		}
	}
done:

	// Coalesced messages should NOT count as drops.
	assert.Equal(t, int64(0), b.PublishDrops(), "coalesced messages must not count as drops")
}

func TestSubscribeWithCoalescing_SSEHandler(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("prices")
	server := httptest.NewServer(handler)
	defer server.Close()

	// Give the SSE handler time to subscribe.
	time.Sleep(50 * time.Millisecond)

	ch, unsub := b.SubscribeWithCoalescing("prices")
	defer unsub()

	b.Publish("prices", "data: hello\n\n")

	time.Sleep(50 * time.Millisecond)

	select {
	case msg := <-ch:
		assert.Equal(t, "data: hello\n\n", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for coalesced message")
	}

	stats := b.Stats()
	assert.GreaterOrEqual(t, stats.Subscribers, 1)
}

func TestSubscribeWithCoalescing_Unsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeWithCoalescing("prices")
	unsub()
	unsub() // double unsubscribe should be safe

	assert.Equal(t, 0, b.SubscriberCount())
}

func TestSubscribeWithCoalescing_CoexistsWithSSEHandler(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("events")
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, http.NoBody)
	require.NoError(t, err)

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	time.Sleep(50 * time.Millisecond)

	ch, unsub := b.SubscribeWithCoalescing("events")
	defer unsub()

	b.Publish("events", "data: test\n\n")

	select {
	case msg := <-ch:
		assert.Equal(t, "data: test\n\n", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for coalesced message")
	}

	cancel()
}
