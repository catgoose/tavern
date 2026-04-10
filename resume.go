package tavern

import (
	"context"
	"strings"
	"sync/atomic"
	"time"
)

// ReplayEntry pairs a message with its event ID for Last-Event-ID resumption
// support. When ExpiresAt is non-zero, the entry will be removed from the
// replay cache after that time (see [SSEBroker.PublishWithTTL]).
type ReplayEntry struct {
	// ID is the SSE event identifier used for Last-Event-ID resumption.
	ID string
	// Msg is the raw message payload stored in the replay log.
	Msg string
	// ExpiresAt is when this entry should be purged from the replay cache.
	// A zero value means the entry does not expire.
	ExpiresAt time.Time
	// AutoRemoveID is the DOM element ID to send an OOB delete fragment for
	// when this entry expires. Empty means no auto-removal.
	AutoRemoveID string
	// PublishedAt records when the entry was originally published, used to
	// compute reconnection gap durations.
	PublishedAt time.Time
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
	store := b.replayStore
	if store == nil {
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
	}

	subscribers := b.topics[topic]
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.Unlock()

	if store != nil {
		_ = store.Append(context.Background(), topic, ReplayEntry{ID: id, Msg: msg, PublishedAt: time.Now()})
	}

	wireMsg := injectSSEID(msg, id)
	sent, dropped := b.publishToChannels(topic, channels, wireMsg)
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
	return b.subscribeFromIDInternal(topic, lastEventID, subscribeConfig{})
}

// subscribeFromIDInternal is the shared implementation behind
// [SSEBroker.SubscribeFromID] and [SSEBroker.SubscribeFromIDWith]. It handles
// registration, replay, reconnect control events, gap fallback, and delivery
// of replay messages. Rate limiting is applied by callers (never to replay
// messages).
func (b *SSEBroker) subscribeFromIDInternal(topic, lastEventID string, cfg subscribeConfig) (msgs <-chan string, unsubscribe func()) {
	// Pre-compute the fresh-subscribe snapshot outside the lock to mirror
	// SubscribeWith's atomic ordering.
	var preSnap string
	if cfg.snapshotFn != nil && lastEventID == "" {
		preSnap = cfg.snapshotFn()
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan string)
		close(ch)
		return ch, func() {}
	}
	if !b.admitSubscriber(topic) {
		b.mu.Unlock()
		return nil, nil
	}
	ch := make(chan string, b.bufferSize)
	b.chanGuards.Store(ch, &chanGuard{})

	// Inject snapshot BEFORE registering so no live publish can race ahead.
	// Only applies to fresh subscribes; resumes rely on replay instead.
	if preSnap != "" {
		select {
		case ch <- preSnap:
		default:
		}
	}

	scoped := cfg.scope != ""
	if scoped {
		if b.scopedTopics[topic] == nil {
			b.scopedTopics[topic] = make(map[chan string]scopedSub)
		}
		b.scopedTopics[topic][ch] = scopedSub{scope: cfg.scope}
		b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, Scope: cfg.scope, ConnectedAt: time.Now()}
	} else {
		if b.topics[topic] == nil {
			b.topics[topic] = make(map[chan string]struct{})
		}
		b.topics[topic][ch] = struct{}{}
		b.subscriberMeta[ch] = &SubscriberInfo{Topic: topic, ConnectedAt: time.Now()}
	}

	if cfg.meta != nil {
		if info := b.subscriberMeta[ch]; info != nil {
			info.ID = cfg.meta.ID
			info.Meta = cfg.meta.Meta
		}
	}

	if cfg.filter != nil {
		if b.filterPredicates == nil {
			b.filterPredicates = make(map[chan string]FilterPredicate)
		}
		b.filterPredicates[ch] = cfg.filter
	}

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
	store := b.replayStore
	var replayMsgs []string
	var gapDetected bool
	if store == nil {
		now := time.Now()
		if lastEventID == "" {
			// No Last-Event-ID: replay all cached (same as regular Subscribe).
			// Filter out expired entries from the replay log if present.
			if log := b.replayLog[topic]; len(log) > 0 {
				for _, e := range log {
					if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
						continue
					}
					replayMsgs = append(replayMsgs, injectSSEID(e.Msg, e.ID))
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
						replayMsgs = append(replayMsgs, injectSSEID(e.Msg, e.ID))
					}
					break
				}
			}
			if !found {
				gapDetected = true
			}
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

	// When an external ReplayStore is configured, query it outside the lock.
	if store != nil {
		if lastEventID == "" {
			entries, _ := store.Latest(context.Background(), topic, b.replayStoreLimit(topic))
			for _, e := range entries {
				if e.ID != "" {
					replayMsgs = append(replayMsgs, injectSSEID(e.Msg, e.ID))
				} else {
					replayMsgs = append(replayMsgs, e.Msg)
				}
			}
		} else {
			entries, found, _ := store.AfterID(context.Background(), topic, lastEventID, 0)
			if !found {
				gapDetected = true
				// Re-read gap callbacks under the lock.
				b.mu.RLock()
				gapCallbacks = b.onReplayGap[topic]
				gapStrategy = b.replayGapStrategy[topic]
				gapSnapshotFn = b.replayGapSnapshot[topic]
				b.mu.RUnlock()
			}
			for _, e := range entries {
				replayMsgs = append(replayMsgs, injectSSEID(e.Msg, e.ID))
			}
		}
	}

	// Apply replay filter: filter predicate applies uniformly to replay and
	// live messages. Control events are generated inline and bypass the
	// filter (they are never stored in filterPredicates).
	if cfg.filter != nil && len(replayMsgs) > 0 {
		filtered := replayMsgs[:0]
		for _, msg := range replayMsgs {
			if cfg.filter(msg) {
				filtered = append(filtered, msg)
			}
		}
		replayMsgs = filtered
	}

	// Send reconnected control event when Last-Event-ID is present.
	if isReconnect {
		controlMsg := reconnectedControlEvent()
		select {
		case ch <- controlMsg:
		default:
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
	var replayDelivered, replayDropped int
	if bundle && len(replayMsgs) > 0 {
		var bundled strings.Builder
		for _, msg := range replayMsgs {
			bundled.WriteString(msg)
		}
		select {
		case ch <- bundled.String():
			replayDelivered = len(replayMsgs)
		default:
			replayDropped = len(replayMsgs)
		}
	} else {
		for _, msg := range replayMsgs {
			select {
			case ch <- msg:
				replayDelivered++
			default:
				replayDropped++
			}
		}
	}

	// Populate replay delivery stats and fire reconnect callbacks.
	if isReconnect {
		reconnectInfo.ReplayDelivered = replayDelivered
		reconnectInfo.ReplayDropped = replayDropped
		for _, fn := range reconnectCallbacks {
			go fn(reconnectInfo)
		}
		// Emit truncation control event when replay messages were dropped.
		if replayDropped > 0 {
			select {
			case ch <- replayTruncatedControlEvent(replayDelivered, replayDropped):
			default:
			}
		}
	}

	unsub := b.makeSubscribeFromIDUnsubscribe(topic, ch, scoped)
	return ch, unsub
}

// makeSubscribeFromIDUnsubscribe builds an unsubscribe closure for
// subscriptions created via [SSEBroker.subscribeFromIDInternal]. It cleans up
// topic/scoped maps, subscriber metadata, filter predicates, and drop counts.
func (b *SSEBroker) makeSubscribeFromIDUnsubscribe(topic string, ch chan string, scoped bool) func() {
	return func() {
		b.mu.Lock()
		var lastHooks []func(string)
		var removed bool
		if scoped {
			if _, ok := b.scopedTopics[topic][ch]; ok {
				delete(b.scopedTopics[topic], ch)
				removed = true
			}
		} else {
			if _, ok := b.topics[topic][ch]; ok {
				delete(b.topics[topic], ch)
				removed = true
			}
		}
		if removed {
			delete(b.subscriberMeta, ch)
			delete(b.filterPredicates, ch)
			b.closeChan(ch)
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

// SubscribeFromIDWith creates a composable resume-aware subscription. When
// lastEventID is empty it behaves like [SSEBroker.SubscribeWith] with
// replay-cache replay. When lastEventID is non-empty it replays messages from
// the ID-backed replay log after that ID, emits a tavern-reconnected control
// event, and handles gap fallback consistently with
// [SSEBroker.SubscribeFromID].
//
// Option semantics:
//
//   - [SubWithFilter]:   applies uniformly to replay AND live messages.
//     Control events (tavern-reconnected, tavern-replay-gap,
//     tavern-replay-truncated) always bypass the filter.
//   - [SubWithMeta]:     applied the same as live subscriptions.
//   - [SubWithSnapshot]: applied ONLY on fresh subscribe (lastEventID is
//     empty); never delivered on successful resume. Gap-fallback snapshot
//     is a separate mechanism configured via
//     [SSEBroker.SetReplayGapPolicy].
//   - [SubWithRate]:     applied to LIVE delivery only. Replay messages are
//     delivered directly and are NOT rate-limited.
//   - [SubWithScope]:    sets the subscriber scope for live scope filtering.
//     The replay log is scope-less, so any replayed messages are delivered
//     regardless of scope. Note that [SSEBroker.PublishWithID] publishes only
//     to unscoped subscribers, so a scoped resume subscriber will only receive
//     live messages via scope-aware publish paths such as
//     [SSEBroker.PublishTo].
func (b *SSEBroker) SubscribeFromIDWith(topic, lastEventID string, opts ...SubscribeOption) (msgs <-chan string, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	ch, unsub := b.subscribeFromIDInternal(topic, lastEventID, cfg)
	if ch == nil {
		return nil, nil
	}

	// Apply rate limiting to LIVE delivery only. Replay has already been
	// written directly to the channel above and is not affected.
	if cfg.rate != nil {
		interval := cfg.rate.interval()
		if interval > 0 {
			biCh := b.findBiChanAny(topic, ch)
			if biCh != nil {
				b.rateLimit.add(biCh, topic, interval)
				origUnsub := unsub
				unsub = func() {
					b.rateLimit.remove(biCh)
					origUnsub()
				}
			}
		}
	}

	return ch, unsub
}
