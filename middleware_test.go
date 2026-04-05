package tavern

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareGlobal(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, strings.ToUpper(msg))
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "HELLO", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewareChainOrder(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// First registered = outermost. Outermost runs first, transforms msg,
	// then passes to inner. So first prepends "[first]", second sees that
	// and prepends "[second]", producing "[second][first]x".
	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[first]"+msg)
		}
	})
	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[second]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "x")

	select {
	case msg := <-ch:
		assert.Equal(t, "[second][first]x", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewareSwallow(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			// swallow — don't call next
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "should not arrive")

	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestMiddlewareTopicScoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.UseTopics("orders:*", func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[orders]"+msg)
		}
	})

	chOrders, unsubOrders := b.Subscribe("orders:list")
	defer unsubOrders()

	chOther, unsubOther := b.Subscribe("users:list")
	defer unsubOther()

	b.Publish("orders:list", "data")
	b.Publish("users:list", "data")

	select {
	case msg := <-chOrders:
		assert.Equal(t, "[orders]data", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on orders")
	}

	select {
	case msg := <-chOther:
		assert.Equal(t, "data", msg, "non-matching topic should not have middleware applied")
	case <-time.After(time.Second):
		t.Fatal("timed out on users")
	}
}

func TestMiddlewareGlobalAndTopicCombined(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[global]"+msg)
		}
	})
	b.UseTopics("admin:*", func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[admin]"+msg)
		}
	})

	ch, unsub := b.Subscribe("admin:users")
	defer unsub()

	b.Publish("admin:users", "x")

	select {
	case msg := <-ch:
		// global outermost runs first, topic inner runs second
		assert.Equal(t, "[admin][global]x", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.SubscribeScoped("t", "user1")
	defer unsub()

	b.PublishTo("t", "user1", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishOOB(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.PublishOOB("t", Replace("el", "<p>hi</p>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "[mw]")
		assert.Contains(t, msg, "hx-swap-oob")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishOOBTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.SubscribeScoped("t", "s1")
	defer unsub()

	b.PublishOOBTo("t", "s1", Replace("el", "<p>hi</p>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "[mw]")
		assert.Contains(t, msg, "hx-swap-oob")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishWithReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.PublishWithReplay("t", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishIfChanged(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok := b.PublishIfChanged("t", "hello")
	require.True(t, ok)

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishIfChangedTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.SubscribeScoped("t", "s1")
	defer unsub()

	ok := b.PublishIfChangedTo("t", "s1", "hello")
	require.True(t, ok)

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishThrottled(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.PublishThrottled("t", "hello", time.Second)

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewarePublishDebounced(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[mw]"+msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.PublishDebounced("t", "hello", 10*time.Millisecond)

	select {
	case msg := <-ch:
		assert.Equal(t, "[mw]hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		topic   string
		want    bool
	}{
		{"orders:*", "orders:list", true},
		{"orders:*", "orders:detail", true},
		{"orders:*", "orders:detail:item", false},
		{"*:list", "orders:list", true},
		{"*:*", "orders:list", true},
		{"orders:list", "orders:list", true},
		{"orders:list", "orders:detail", false},
		{"admin", "admin", true},
		{"admin", "admin:users", false},
		{"*", "anything", true},
		{"a:b:*", "a:b:c", true},
		{"a:b:*", "a:b", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.topic, func(t *testing.T) {
			assert.Equal(t, tt.want, matchPattern(tt.pattern, tt.topic))
		})
	}
}

func TestMiddlewareSideEffect(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var logged atomic.Int64
	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			logged.Add(1)
			next(topic, msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "a")
	b.Publish("t", "b")

	<-ch
	<-ch

	assert.Equal(t, int64(2), logged.Load())
}

func TestMiddlewareConcurrentPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var count atomic.Int64
	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			count.Add(1)
			next(topic, msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			b.Publish("t", "msg")
		}()
	}
	wg.Wait()

	// Drain what we can.
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			goto done
		}
	}
done:
	assert.Equal(t, int64(n), count.Load())
	assert.Greater(t, drained, 0)
}

func TestMiddlewareNoMiddleware(t *testing.T) {
	// Ensure publish works normally with no middleware registered.
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	b.Publish("t", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMiddlewareTopicSwallow(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.UseTopics("admin:*", func(_ PublishFunc) PublishFunc {
		return func(_, _ string) {
			// swallow all admin publishes
		}
	})

	ch, unsub := b.Subscribe("admin:secret")
	defer unsub()

	chPublic, unsubPublic := b.Subscribe("public:news")
	defer unsubPublic()

	b.Publish("admin:secret", "classified")
	b.Publish("public:news", "headline")

	// admin should be swallowed
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message on admin topic: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// public should go through
	select {
	case msg := <-chPublic:
		assert.Equal(t, "headline", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on public topic")
	}
}

func TestMiddlewareMultipleTopicPatterns(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.UseTopics("a:*", func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[a]"+msg)
		}
	})
	b.UseTopics("*:x", func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, "[x]"+msg)
		}
	})

	ch, unsub := b.Subscribe("a:x")
	defer unsub()

	b.Publish("a:x", "data")

	select {
	case msg := <-ch:
		// Both topic middlewares match; first registered is outermost
		assert.Equal(t, "[x][a]data", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}
