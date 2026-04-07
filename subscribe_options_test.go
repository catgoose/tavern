package tavern

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeWith_FilterAndRate(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeWith("topic",
		SubWithFilter(func(msg string) bool {
			return strings.HasPrefix(msg, "keep:")
		}),
		SubWithRate(Rate{MaxPerSecond: 1000}),
	)
	require.NotNil(t, ch)
	defer unsub()

	b.Publish("topic", "keep:hello")
	b.Publish("topic", "drop:this")
	b.Publish("topic", "keep:world")

	var received []string
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			received = append(received, msg)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	assert.Equal(t, []string{"keep:hello", "keep:world"}, received)
}

func TestSubscribeWith_ScopeAndFilter(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeWith("topic",
		SubWithScope("user:1"),
		SubWithFilter(func(msg string) bool {
			return msg != "skip"
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	b.PublishTo("topic", "user:1", "hello")
	b.PublishTo("topic", "user:1", "skip")
	b.PublishTo("topic", "user:2", "other")
	b.PublishTo("topic", "user:1", "world")

	var received []string
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			received = append(received, msg)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	assert.Equal(t, []string{"hello", "world"}, received)
}

func TestSubscribeWith_ScopeFilterAndRate(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeWith("topic",
		SubWithScope("s"),
		SubWithFilter(func(msg string) bool {
			return msg != "skip"
		}),
		SubWithRate(Rate{MaxPerSecond: 10000}),
	)
	require.NotNil(t, ch)
	defer unsub()

	b.PublishTo("topic", "s", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestSubscribeWith_Meta(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	_, unsub := b.SubscribeWith("topic",
		SubWithMeta(SubscribeMeta{
			ID:   "sub-1",
			Meta: map[string]string{"role": "admin"},
		}),
	)
	defer unsub()

	subs := b.Subscribers("topic")
	require.Len(t, subs, 1)
	assert.Equal(t, "sub-1", subs[0].ID)
	assert.Equal(t, "admin", subs[0].Meta["role"])
}

func TestSubscribeWith_Snapshot(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeWith("topic",
		SubWithSnapshot(func() string { return "initial-state" }),
	)
	require.NotNil(t, ch)
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "initial-state", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for snapshot")
	}
}

func TestSubscribeMultiWith_FilterApplied(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith(
		[]string{"a", "b"},
		SubWithFilter(func(msg string) bool {
			return msg != "skip"
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	b.Publish("a", "hello")
	b.Publish("b", "skip")
	b.Publish("b", "world")

	var received []TopicMessage
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case tm := <-ch:
			received = append(received, tm)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	require.Len(t, received, 2)
	var datas []string
	for _, tm := range received {
		datas = append(datas, tm.Data)
	}
	assert.Contains(t, datas, "hello")
	assert.Contains(t, datas, "world")
	assert.NotContains(t, datas, "skip")
}

func TestSubscribeGlobWith_FilterAndRate(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	ch, unsub := b.SubscribeGlobWith("app/**",
		SubWithFilter(func(msg string) bool {
			return msg != "skip"
		}),
	)
	require.NotNil(t, ch)
	defer unsub()

	b.Publish("app/users", "hello")
	b.Publish("app/orders", "skip")
	b.Publish("app/orders", "world")

	var received []TopicMessage
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case tm := <-ch:
			received = append(received, tm)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	require.Len(t, received, 2)
	assert.Equal(t, "hello", received[0].Data)
	assert.Equal(t, "world", received[1].Data)
}

func TestPublishBatch_PublishWithTTL(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	b.SetReplayPolicy("topic", 5)

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	batch := b.Batch()
	batch.PublishWithTTL("topic", "ttl-msg", 1*time.Hour)
	// Message should be delivered immediately.

	select {
	case msg := <-ch:
		assert.Equal(t, "ttl-msg", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestPublishBatch_PublishWithID(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	b.SetReplayPolicy("topic", 5)

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	batch := b.Batch()
	batch.PublishWithID("topic", "id-1", "id-msg")

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "id-msg")
		assert.Contains(t, msg, "id: id-1")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestSubscribeWith_AdmissionDenied(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribers(0), WithMaxSubscribersPerTopic(1))
	defer b.Close()

	ch1, unsub1 := b.SubscribeWith("topic")
	require.NotNil(t, ch1)
	defer unsub1()

	ch2, unsub2 := b.SubscribeWith("topic")
	assert.Nil(t, ch2)
	assert.Nil(t, unsub2)
}
