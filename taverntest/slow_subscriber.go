package taverntest

import (
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// SlowSubscriberConfig configures the simulated slow subscriber.
type SlowSubscriberConfig struct {
	// ReadDelay is the time to wait before reading each message from the
	// subscriber channel. This simulates a client that processes messages
	// slowly, causing backpressure.
	ReadDelay time.Duration
}

// SlowSubscriber simulates a subscriber that reads messages slowly, useful
// for testing backpressure, drop-oldest behavior, and slow subscriber
// eviction scenarios. SlowSubscriber is safe for concurrent use by multiple
// goroutines.
type SlowSubscriber struct {
	msgs   []string
	mu     sync.Mutex
	unsub  func()
	done   chan struct{}
	closed bool
}

// NewSlowSubscriber subscribes to the given topic and reads messages with a
// configurable delay between each read. Call [SlowSubscriber.Close] when done.
func NewSlowSubscriber(broker *tavern.SSEBroker, topic string, cfg SlowSubscriberConfig) *SlowSubscriber {
	ch, unsub := broker.Subscribe(topic)
	s := &SlowSubscriber{
		unsub: unsub,
		done:  make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		for msg := range ch {
			if cfg.ReadDelay > 0 {
				time.Sleep(cfg.ReadDelay)
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, msg)
			s.mu.Unlock()
		}
	}()
	return s
}

// Close stops reading and unsubscribes. Safe to call multiple times.
func (s *SlowSubscriber) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.unsub()
	<-s.done
}

// Messages returns a copy of all messages that have been read so far.
func (s *SlowSubscriber) Messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.msgs))
	copy(cp, s.msgs)
	return cp
}

// Len returns the number of messages read so far.
func (s *SlowSubscriber) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// WaitFor blocks until at least n messages have been read or the timeout
// elapses. Returns true if n messages were read, false on timeout.
func (s *SlowSubscriber) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		count := len(s.msgs)
		s.mu.Unlock()
		if count >= n {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

// Done returns a channel that is closed when the subscriber's collection
// goroutine exits (e.g., because the broker closed the channel or the
// subscriber was evicted).
func (s *SlowSubscriber) Done() <-chan struct{} {
	return s.done
}
