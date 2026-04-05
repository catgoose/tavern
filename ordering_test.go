package tavern

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetOrdered_ConcurrentPublishesOrderPreserved(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(200))
	defer b.Close()

	b.SetOrdered("chat", true)

	const numSubs = 3
	const numMsgs = 50

	channels := make([]<-chan string, numSubs)
	unsubs := make([]func(), numSubs)
	for i := 0; i < numSubs; i++ {
		channels[i], unsubs[i] = b.Subscribe("chat")
	}
	defer func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}()

	// Publish concurrently from multiple goroutines.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(goroutine int) {
			defer wg.Done()
			for i := 0; i < numMsgs; i++ {
				b.Publish("chat", fmt.Sprintf("g%d:m%d", goroutine, i))
			}
		}(g)
	}
	wg.Wait()

	// Give subscribers time to receive.
	time.Sleep(50 * time.Millisecond)

	// Collect messages from all subscribers.
	results := make([][]string, numSubs)
	for i := 0; i < numSubs; i++ {
		timeout := time.After(200 * time.Millisecond)
		for {
			select {
			case msg := <-channels[i]:
				results[i] = append(results[i], msg)
			case <-timeout:
				goto nextSub
			}
		}
	nextSub:
	}

	// All subscribers should have the same number of messages in the same order.
	require.NotEmpty(t, results[0], "first subscriber should have received messages")
	for i := 1; i < numSubs; i++ {
		assert.Equal(t, results[0], results[i],
			"subscriber %d should see same order as subscriber 0", i)
	}
}

func TestSetOrdered_NonOrderedUnaffected(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(100))
	defer b.Close()

	// "chat" is ordered, "noise" is not.
	b.SetOrdered("chat", true)

	ch, unsub := b.Subscribe("noise")
	defer unsub()

	// Publishing to non-ordered topic should work normally.
	b.Publish("noise", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for non-ordered message")
	}
}

func TestSetOrdered_ToggleOnOff(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(100))
	defer b.Close()

	b.SetOrdered("topic", true)

	// Verify ordered topics map has the entry.
	b.mu.RLock()
	_, hasEntry := b.orderedTopics["topic"]
	b.mu.RUnlock()
	assert.True(t, hasEntry, "topic should be in orderedTopics")

	b.SetOrdered("topic", false)

	// Verify entry removed.
	b.mu.RLock()
	_, hasEntry = b.orderedTopics["topic"]
	b.mu.RUnlock()
	assert.False(t, hasEntry, "topic should not be in orderedTopics after disabling")

	// Publishing should still work without ordering.
	ch, unsub := b.Subscribe("topic")
	defer unsub()
	b.Publish("topic", "after-toggle")

	select {
	case msg := <-ch:
		assert.Equal(t, "after-toggle", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestSetOrdered_ScopedPublishes(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(100))
	defer b.Close()

	b.SetOrdered("scoped", true)

	ch1, unsub1 := b.SubscribeScoped("scoped", "a")
	defer unsub1()
	ch2, unsub2 := b.SubscribeScoped("scoped", "a")
	defer unsub2()

	const numMsgs = 20
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(goroutine int) {
			defer wg.Done()
			for i := 0; i < numMsgs; i++ {
				b.PublishTo("scoped", "a", fmt.Sprintf("g%d:m%d", goroutine, i))
			}
		}(g)
	}
	wg.Wait()

	time.Sleep(50 * time.Millisecond)

	var r1, r2 []string
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch1:
			r1 = append(r1, msg)
		case <-timeout:
			goto drain2
		}
	}
drain2:
	timeout = time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ch2:
			r2 = append(r2, msg)
		case <-timeout:
			goto done
		}
	}
done:

	assert.Equal(t, r1, r2, "scoped subscribers should see same order")
}
