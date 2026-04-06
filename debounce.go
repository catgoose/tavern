package tavern

import (
	"sync"
	"time"
)

// debouncer tracks per-topic debounce timers.
type debouncer struct {
	mu     sync.Mutex
	timers map[string]*debounceEntry
}

type debounceEntry struct {
	timer *time.Timer
	msg   string
}

// PublishDebounced publishes msg to the topic after the given duration of quiet.
// If called again for the same topic before the duration elapses, the timer
// resets and only the latest message is published. This is useful for rapid
// state changes (typing indicators, slider drags) where intermediate states
// are noise. This method is safe for concurrent use.
func (b *SSEBroker) PublishDebounced(topic, msg string, after time.Duration) {
	b.debounce.mu.Lock()
	defer b.debounce.mu.Unlock()

	if entry, ok := b.debounce.timers[topic]; ok {
		entry.timer.Stop()
		entry.msg = msg
		entry.timer.Reset(after)
		return
	}

	entry := &debounceEntry{msg: msg}
	entry.timer = time.AfterFunc(after, func() {
		b.debounce.mu.Lock()
		m := entry.msg
		delete(b.debounce.timers, topic)
		b.debounce.mu.Unlock()
		b.Publish(topic, m)
	})
	b.debounce.timers[topic] = entry
}
