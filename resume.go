package tavern

import (
	"sync/atomic"
	"time"
)

// ReplayEntry pairs a message with its event ID for resumption support.
type ReplayEntry struct {
	ID  string
	Msg string
}

// PublishWithID publishes msg to the topic with an associated event ID.
// The message is cached in the replay log for Last-Event-ID resumption.
// The replay log size is controlled by SetReplayPolicy (default 1).
func (b *SSEBroker) PublishWithID(topic, id, msg string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	maxSize := 1
	if n, ok := b.replaySize[topic]; ok {
		maxSize = n
	}
	log := b.replayLog[topic]
	log = append(log, ReplayEntry{ID: id, Msg: msg})
	if len(log) > maxSize {
		log = log[len(log)-maxSize:]
	}
	b.replayLog[topic] = log

	// Also update replayCache for regular Subscribe replay compatibility.
	msgs := b.replayCache[topic]
	msgs = append(msgs, msg)
	if len(msgs) > maxSize {
		msgs = msgs[len(msgs)-maxSize:]
	}
	b.replayCache[topic] = msgs

	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	sent, dropped := b.publishToChannels(topic, channels, msg)
	if b.metrics != nil {
		tc := b.metrics.counter(topic)
		tc.published.Add(int64(sent))
		tc.dropped.Add(int64(dropped))
	}
}

// SubscribeFromID subscribes to a topic and replays all cached messages
// with IDs after lastEventID. If lastEventID is empty, all cached messages
// are replayed (same as Subscribe). If lastEventID is not found in the
// replay log, no replay occurs (gap too large) and only live messages
// are delivered.
//
// This implements the server side of the SSE Last-Event-ID resumption
// protocol. The HTTP handler should read the Last-Event-ID header from
// the request and pass it here.
func (b *SSEBroker) SubscribeFromID(topic, lastEventID string) (msgs <-chan string, unsubscribe func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan string, b.bufferSize)
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[chan string]struct{})
	}
	b.topics[topic][ch] = struct{}{}
	total := len(b.topics[topic]) + len(b.scopedTopics[topic])
	var firstHooks []func(string)
	if total == 1 {
		firstHooks = b.onFirst[topic]
	}
	if b.metrics != nil {
		if total > b.metrics.peakSubs[topic] {
			b.metrics.peakSubs[topic] = total
		}
	}

	// Find messages after lastEventID.
	var replayMsgs []string
	if lastEventID == "" {
		// No Last-Event-ID: replay all cached (same as regular Subscribe).
		replayMsgs = append([]string(nil), b.replayCache[topic]...)
	} else {
		log := b.replayLog[topic]
		for i, entry := range log {
			if entry.ID == lastEventID {
				// Replay everything after this entry.
				for _, e := range log[i+1:] {
					replayMsgs = append(replayMsgs, e.Msg)
				}
				break
			}
		}
		// If ID not found, replayMsgs stays nil: gap too large, no replay.
	}

	if b.evictThreshold > 0 {
		b.dropCountsMu.Lock()
		b.dropCounts[ch] = &atomic.Int64{}
		b.dropCountsMu.Unlock()
	}
	delete(b.topicEmpty, topic)
	b.mu.Unlock()

	for _, fn := range firstHooks {
		go fn(topic)
	}
	for _, msg := range replayMsgs {
		select {
		case ch <- msg:
		default:
		}
	}
	return ch, func() {
		b.mu.Lock()
		var lastHooks []func(string)
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
			close(ch)
			total := len(b.topics[topic]) + len(b.scopedTopics[topic])
			if total == 0 {
				lastHooks = b.onLast[topic]
				b.topicEmpty[topic] = time.Now()
			}
		}
		b.mu.Unlock()
		if b.evictThreshold > 0 {
			b.dropCountsMu.Lock()
			delete(b.dropCounts, ch)
			b.dropCountsMu.Unlock()
		}
		for _, fn := range lastHooks {
			go fn(topic)
		}
	}
}
