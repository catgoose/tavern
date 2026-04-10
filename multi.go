package tavern

import "sync"

// TopicMessage pairs a message with the topic it was published on. It is
// returned by multiplexed subscription methods such as [SSEBroker.SubscribeMulti]
// and [SSEBroker.SubscribeGlob].
type TopicMessage struct {
	// Topic is the name of the topic the message was published to.
	Topic string
	// Data is the published message payload.
	Data string
}

// SubscribeMulti subscribes to multiple topics and returns a single channel
// that receives [TopicMessage] values tagged with their source topic. The
// returned unsubscribe function removes the subscriber from all topics at
// once. Each topic counts toward its own subscriber total (lifecycle hooks
// fire correctly).
//
// This eliminates the need for reflect.Select when a single SSE connection
// serves multiple topics.
func (b *SSEBroker) SubscribeMulti(topics ...string) (msgs <-chan TopicMessage, unsubscribe func()) {
	out := make(chan TopicMessage, b.bufferSize)
	unsubs := make([]func(), 0, len(topics))
	done := make(chan struct{})
	var wg sync.WaitGroup

	for _, topic := range topics {
		ch, unsub := b.Subscribe(topic)
		unsubs = append(unsubs, unsub)
		t := topic
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- TopicMessage{Topic: t, Data: msg}:
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	var closeOnce sync.Once
	closeOut := func() {
		closeOnce.Do(func() { close(out) })
	}

	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			for _, unsub := range unsubs {
				unsub()
			}
		})
	}

	// Close out when all fan-in goroutines exit (e.g. broker closed).
	go func() {
		wg.Wait()
		closeOut()
	}()

	return out, func() {
		doUnsub()
		wg.Wait()
		closeOut()
	}
}

// SubscribeMultiFromID subscribes to multiple topics with shared Last-Event-ID
// replay/resume semantics and returns a single channel of TopicMessage values
// tagged with their source topic. Each topic independently replays from its
// ID-backed replay log after lastEventID (or from the current cache if
// lastEventID is empty), then continues with live messages. The returned
// unsubscribe function closes all inner subscriptions at once.
//
// Semantics:
//   - The same lastEventID is applied uniformly to every topic. If a topic's
//     replay log does not contain the ID, that topic's gap handling (reconnect
//     callbacks, gap strategy) runs as configured via SetReplayGapPolicy.
//   - Ordering is not guaranteed across topics. Within a single topic,
//     messages preserve their published order.
//   - Calling the returned unsubscribe closes all inner channels; the output
//     channel is closed once all fan-in goroutines exit.
//
// This is the multi-topic counterpart to [SSEBroker.SubscribeFromID] and
// mirrors [SSEBroker.SubscribeMulti]'s fan-in structure.
func (b *SSEBroker) SubscribeMultiFromID(topics []string, lastEventID string) (msgs <-chan TopicMessage, unsubscribe func()) {
	out := make(chan TopicMessage, b.bufferSize)
	unsubs := make([]func(), 0, len(topics))
	done := make(chan struct{})
	var wg sync.WaitGroup

	for _, topic := range topics {
		ch, unsub := b.SubscribeFromID(topic, lastEventID)
		if ch == nil {
			continue
		}
		unsubs = append(unsubs, unsub)
		t := topic
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- TopicMessage{Topic: t, Data: msg}:
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	var closeOnce sync.Once
	closeOut := func() {
		closeOnce.Do(func() { close(out) })
	}

	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			for _, unsub := range unsubs {
				unsub()
			}
		})
	}

	// Close out when all fan-in goroutines exit (e.g. broker closed).
	go func() {
		wg.Wait()
		closeOut()
	}()

	return out, func() {
		doUnsub()
		wg.Wait()
		closeOut()
	}
}
