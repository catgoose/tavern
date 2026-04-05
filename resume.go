package tavern

import (
	"strings"
	"sync/atomic"
	"time"
)

// ReplayEntry pairs a message with its event ID for resumption support.
// When ExpiresAt is non-zero, the entry will be removed from the replay
// cache after that time (see [SSEBroker.PublishWithTTL]).
type ReplayEntry struct {
	ID           string
	Msg          string
	ExpiresAt    time.Time // zero means no expiry
	AutoRemoveID string    // element ID for OOB delete on expiry (empty = none)
	PublishedAt  time.Time
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
	log = append(log, ReplayEntry{ID: id, Msg: msg, PublishedAt: time.Now()})
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
	b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, ConnectedAt: time.Now()}
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

	// Find messages after lastEventID, skipping expired TTL entries.
	now := time.Now()
	var replayMsgs []string
	var gapDetected bool
	if lastEventID == "" {
		// No Last-Event-ID: replay all cached (same as regular Subscribe).
		// Filter out expired entries from the replay log if present.
		if log := b.replayLog[topic]; len(log) > 0 {
			for _, e := range log {
				if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
					continue
				}
				replayMsgs = append(replayMsgs, e.Msg)
			}
		} else {
			replayMsgs = append([]string(nil), b.replayCache[topic]...)
		}
	} else {
		log := b.replayLog[topic]
		found := false
		for i, entry := range log {
			if entry.ID == lastEventID {
				found = true
				// Replay everything after this entry, skipping expired.
				for _, e := range log[i+1:] {
					if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
						continue
					}
					replayMsgs = append(replayMsgs, e.Msg)
				}
				break
			}
		}
		if !found {
			gapDetected = true
		}
	}

	// Collect gap handling state while still holding the lock.
	var gapCallbacks []ReplayGapCallback
	var gapStrategy GapStrategy
	var gapSnapshotFn func() string
	if gapDetected {
		gapCallbacks = b.onReplayGap[topic]
		gapStrategy = b.replayGapStrategy[topic]
		gapSnapshotFn = b.replayGapSnapshot[topic]
	}
	subInfo := b.subscriberMeta[ch]

	// Collect reconnect state while holding the lock.
	isReconnect := lastEventID != ""
	var reconnectCallbacks []ReconnectCallback
	var reconnectInfo ReconnectInfo
	var bundle bool
	if isReconnect {
		reconnectCallbacks = b.onReconnect[topic]
		reconnectInfo = b.buildReconnectInfo(topic, lastEventID, ch)
		bundle = b.bundleOnReconnect[topic]
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
	b.publishConnectionEvent("subscribe", topic, total)

	// Send reconnected control event when Last-Event-ID is present.
	if isReconnect {
		controlMsg := reconnectedControlEvent()
		select {
		case ch <- controlMsg:
		default:
		}
		// Fire reconnect callbacks in their own goroutines.
		for _, fn := range reconnectCallbacks {
			go fn(reconnectInfo)
		}
	}

	// Handle replay gap: fire callbacks, optionally send control event and snapshot.
	if gapDetected {
		// Fire registered gap callbacks regardless of strategy.
		for _, fn := range gapCallbacks {
			go fn(subInfo, lastEventID)
		}
		// For non-silent strategies, send control event and apply the strategy.
		if gapStrategy != GapSilent {
			controlMsg := replayGapControlEvent(lastEventID)
			select {
			case ch <- controlMsg:
			default:
			}
			if gapStrategy == GapFallbackToSnapshot && gapSnapshotFn != nil {
				if snap := gapSnapshotFn(); snap != "" {
					select {
					case ch <- snap:
					default:
					}
				}
			}
		}
	}

	// Deliver replay messages, optionally bundled into a single write.
	if bundle && len(replayMsgs) > 0 {
		var bundled strings.Builder
		for _, msg := range replayMsgs {
			bundled.WriteString(msg)
		}
		select {
		case ch <- bundled.String():
		default:
		}
	} else {
		for _, msg := range replayMsgs {
			select {
			case ch <- msg:
			default:
			}
		}
	}
	return ch, func() {
		b.mu.Lock()
		var lastHooks []func(string)
		if _, ok := b.topics[topic][ch]; ok {
			delete(b.topics[topic], ch)
			delete(b.subscriberMeta, ch)
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
		b.publishConnectionEvent("unsubscribe", topic, -1)
	}
}
