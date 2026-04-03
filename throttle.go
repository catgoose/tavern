package tavern

import (
	"sync"
	"time"
)

// throttler tracks per-topic throttle state.
type throttler struct {
	mu    sync.Mutex
	state map[string]*throttleEntry
}

type throttleEntry struct {
	lastSent time.Time
	pending  *string
	timer    *time.Timer
}

// PublishThrottled publishes msg to the topic at most once per interval.
// The first call publishes immediately. Subsequent calls within the interval
// are held; when the interval elapses, the most recent held message is
// published. This guarantees bounded latency for the first message while
// rate-limiting subsequent ones.
func (b *SSEBroker) PublishThrottled(topic, msg string, interval time.Duration) {
	now := time.Now()
	b.throttle.mu.Lock()

	entry, ok := b.throttle.state[topic]
	if !ok {
		// First call — publish immediately.
		entry = &throttleEntry{lastSent: now}
		b.throttle.state[topic] = entry
		b.throttle.mu.Unlock()
		b.Publish(topic, msg)
		return
	}

	elapsed := now.Sub(entry.lastSent)
	if elapsed >= interval {
		// Interval has passed — publish immediately.
		entry.lastSent = now
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		entry.pending = nil
		b.throttle.mu.Unlock()
		b.Publish(topic, msg)
		return
	}

	// Within interval — hold the message.
	entry.pending = &msg
	if entry.timer == nil {
		remaining := interval - elapsed
		entry.timer = time.AfterFunc(remaining, func() {
			b.throttle.mu.Lock()
			e := b.throttle.state[topic]
			if e != nil && e.pending != nil {
				m := *e.pending
				e.pending = nil
				e.timer = nil
				e.lastSent = time.Now()
				b.throttle.mu.Unlock()
				b.Publish(topic, m)
			} else {
				if e != nil {
					e.timer = nil
				}
				b.throttle.mu.Unlock()
			}
		})
	}
	b.throttle.mu.Unlock()
}
