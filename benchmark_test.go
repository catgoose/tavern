package tavern

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// drain starts a goroutine that reads from ch until it is closed.
// Returns a stop function that blocks until draining is done.
func drain(ch <-chan string) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()
	return func() { <-done }
}

// benchPublishFanOut benchmarks Publish throughput with n subscribers.
func benchPublishFanOut(b *testing.B, n int) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	var stops []func()
	for i := 0; i < n; i++ {
		ch, unsub := broker.Subscribe("bench")
		b.Cleanup(unsub)
		stops = append(stops, drain(ch))
	}

	msg := "hello"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", msg)
	}
	b.StopTimer()
	broker.Close()
	for _, stop := range stops {
		stop()
	}
}

func BenchmarkPublish_1Sub(b *testing.B)    { benchPublishFanOut(b, 1) }
func BenchmarkPublish_10Sub(b *testing.B)   { benchPublishFanOut(b, 10) }
func BenchmarkPublish_100Sub(b *testing.B)  { benchPublishFanOut(b, 100) }
func BenchmarkPublish_1000Sub(b *testing.B) { benchPublishFanOut(b, 1000) }

func BenchmarkPublishOOB(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	frag := Replace("counter", "<span>1</span>")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.PublishOOB("bench", frag)
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublishLatency(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)

	var totalNs int64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		broker.Publish("bench", "latency")
		<-ch
		totalNs += time.Since(start).Nanoseconds()
	}
	b.StopTimer()
	b.ReportMetric(float64(totalNs)/float64(b.N), "ns/publish-to-recv")
}

func BenchmarkSubscribeMemory(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(10))
	b.Cleanup(func() { broker.Close() })

	var m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	const count = 1000
	unsubs := make([]func(), count)
	stops := make([]func(), count)
	for i := 0; i < count; i++ {
		ch, unsub := broker.Subscribe("bench")
		unsubs[i] = unsub
		stops[i] = drain(ch)
	}

	runtime.GC()
	runtime.ReadMemStats(&m2)

	bytesPerSub := float64(m2.HeapAlloc-m1.HeapAlloc) / float64(count)
	b.ReportMetric(bytesPerSub, "bytes/subscriber")

	b.StopTimer()
	for _, unsub := range unsubs {
		unsub()
	}
	broker.Close()
	for _, stop := range stops {
		stop()
	}
}

func BenchmarkPublish_Baseline(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithMiddleware(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	broker.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			next(topic, msg)
		}
	})

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithObservability(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(
		WithBufferSize(256),
		WithObservability(ObservabilityConfig{
			PublishLatency:  true,
			SubscriberLag:   true,
			TopicThroughput: true,
		}),
	)
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithBackpressure(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(
		WithBufferSize(256),
		WithAdaptiveBackpressure(AdaptiveBackpressure{
			ThrottleAt:   5,
			SimplifyAt:   10,
			DisconnectAt: 20,
		}),
	)
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithFilter(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.SubscribeWithFilter("bench", func(msg string) bool {
		return strings.Contains(msg, "pass")
	})
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "pass-msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithOrdering(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	broker.SetOrdered("bench", true)

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublish_WithCoalescing(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.SubscribeWithCoalescing("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.Publish("bench", "msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkConcurrentPublish(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(1024))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	procs := runtime.GOMAXPROCS(0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			broker.Publish("bench", "msg")
		}
	})
	b.StopTimer()
	_ = procs
	broker.Close()
	stop()
}

func BenchmarkConcurrentPublish_Ordered(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(1024))
	b.Cleanup(func() { broker.Close() })

	broker.SetOrdered("bench", true)

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			broker.Publish("bench", "msg")
		}
	})
	b.StopTimer()
	broker.Close()
	stop()
}

func benchBatchFlush(b *testing.B, batchSize int) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(1024))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	msgs := make([]string, batchSize)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("msg-%d", i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := broker.Batch()
		for _, m := range msgs {
			batch.Publish("bench", m)
		}
		batch.Flush()
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkBatchFlush_10(b *testing.B)  { benchBatchFlush(b, 10) }
func BenchmarkBatchFlush_100(b *testing.B) { benchBatchFlush(b, 100) }

func BenchmarkPublishTo_Scoped(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.SubscribeScoped("bench", "user-42")
	b.Cleanup(unsub)
	stop := drain(ch)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.PublishTo("bench", "user-42", "scoped-msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublishIfChanged_Same(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	// Seed with the message so subsequent calls are dedup hits.
	broker.PublishIfChanged("bench", "same-msg")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.PublishIfChanged("bench", "same-msg")
	}
	b.StopTimer()
	broker.Close()
	stop()
}

func BenchmarkPublishIfChanged_Different(b *testing.B) {
	b.ReportAllocs()
	broker := NewSSEBroker(WithBufferSize(256))
	b.Cleanup(func() { broker.Close() })

	ch, unsub := broker.Subscribe("bench")
	b.Cleanup(unsub)
	stop := drain(ch)

	msgs := make([]string, b.N)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("msg-%d", i)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		received := 0
		for range ch {
			received++
			if received >= b.N {
				return
			}
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		broker.PublishIfChanged("bench", msgs[i])
	}
	b.StopTimer()

	// Unsubscribe before waiting to unblock the goroutine.
	unsub()
	wg.Wait()
	broker.Close()
	_ = stop
}
