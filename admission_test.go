package tavern

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithMaxSubscribers_GlobalLimit(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribers(2))
	defer b.Close()

	ch1, unsub1 := b.Subscribe("a")
	require.NotNil(t, ch1)
	defer unsub1()

	ch2, unsub2 := b.Subscribe("b")
	require.NotNil(t, ch2)
	defer unsub2()

	// Third subscriber should be denied.
	ch3, unsub3 := b.Subscribe("c")
	assert.Nil(t, ch3)
	assert.Nil(t, unsub3)

	// After unsubscribing one, a new subscriber should be allowed.
	unsub1()
	ch4, unsub4 := b.Subscribe("c")
	require.NotNil(t, ch4)
	unsub4()
}

func TestWithMaxSubscribersPerTopic(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribersPerTopic(2))
	defer b.Close()

	ch1, unsub1 := b.Subscribe("topic")
	require.NotNil(t, ch1)
	defer unsub1()

	ch2, unsub2 := b.SubscribeScoped("topic", "scope1")
	require.NotNil(t, ch2)
	defer unsub2()

	// Third subscriber on same topic should be denied.
	ch3, unsub3 := b.Subscribe("topic")
	assert.Nil(t, ch3)
	assert.Nil(t, unsub3)

	// Different topic should be fine.
	ch4, unsub4 := b.Subscribe("other-topic")
	require.NotNil(t, ch4)
	unsub4()
}

func TestWithAdmissionControl_CustomCallback(t *testing.T) {
	denied := false
	b := NewSSEBroker(WithAdmissionControl(func(topic string, currentCount int) bool {
		if topic == "vip" && currentCount >= 1 {
			denied = true
			return false
		}
		return true
	}))
	defer b.Close()

	ch1, unsub1 := b.Subscribe("vip")
	require.NotNil(t, ch1)
	defer unsub1()

	// Second subscriber to "vip" should be denied by custom fn.
	ch2, unsub2 := b.Subscribe("vip")
	assert.Nil(t, ch2)
	assert.Nil(t, unsub2)
	assert.True(t, denied)

	// Other topics should be fine.
	ch3, unsub3 := b.Subscribe("public")
	require.NotNil(t, ch3)
	unsub3()
}

func TestAdmissionControl_SSEHandler503(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribers(0), WithMaxSubscribersPerTopic(1))
	defer b.Close()

	// Fill the topic limit.
	ch, unsub := b.Subscribe("events")
	require.NotNil(t, ch)
	defer unsub()

	handler := b.SSEHandler("events")
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestAdmissionControl_ConcurrentBoundary(t *testing.T) {
	const limit = 5
	b := NewSSEBroker(WithMaxSubscribers(limit))
	defer b.Close()

	var mu sync.Mutex
	var successes int
	var failures int
	var unsubs []func()

	var wg sync.WaitGroup
	// Try to create limit+5 subscribers concurrently.
	for i := 0; i < limit+5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := b.Subscribe("topic")
			mu.Lock()
			if ch != nil {
				successes++
				unsubs = append(unsubs, unsub)
			} else {
				failures++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, limit, successes, "exactly %d subscribers should be admitted", limit)
	assert.Equal(t, 5, failures, "excess subscribers should be denied")

	for _, unsub := range unsubs {
		unsub()
	}
}

func TestAdmissionControl_ScopedSubscribers(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribersPerTopic(1))
	defer b.Close()

	ch1, unsub1 := b.SubscribeScoped("topic", "a")
	require.NotNil(t, ch1)
	defer unsub1()

	// Second scoped subscriber should be denied.
	ch2, unsub2 := b.SubscribeScoped("topic", "b")
	assert.Nil(t, ch2)
	assert.Nil(t, unsub2)
}

func TestAdmissionControl_FilterSubscribers(t *testing.T) {
	b := NewSSEBroker(WithMaxSubscribers(1))
	defer b.Close()

	ch1, unsub1 := b.SubscribeWithFilter("topic", func(msg string) bool { return true })
	require.NotNil(t, ch1)
	defer unsub1()

	// Second subscriber should be denied.
	ch2, unsub2 := b.Subscribe("other")
	assert.Nil(t, ch2)
	assert.Nil(t, unsub2)
}
