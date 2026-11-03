package tavern

import (
	"context"
	"strings"
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
		b.appendReplay(store, topic, ReplayEntry{ID: id, Msg: msg, PublishedAt: time.Now()})
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
// of replay messages. Replay and control events are written directly, so a
// rate limit in cfg applies to live delivery only.
func (b *SSEBroker) subscribeFromIDInternal(topic, lastEventID string, cfg subscribeConfig) (msgs <-chan string, unsubscribe func()) {
	// Pre-compute the fresh-subscribe snapshot outside the lock to mirror
	// SubscribeWith's atomic ordering.
	var preSnap string
	if cfg.snapshotFn != nil && lastEventID == "" {
		preSnap = cfg.snapshotFn()
	}

	// The loss policy, filter, metadata and rate limit are attached before
	// the channel is registered so the first concurrent publish sees them.
	// The snapshot is queued first; only fresh subscribes have one, resumes
	// rely on replay instead.
	spec := cfg.spec()
	spec.preload = preSnap
	b.mu.Lock()
	j, closed := b.registerLocked(topic, spec)
	if j == nil {
		b.mu.Unlock()
		return rejectedSubscription(closed, spec.status, topic)
	}
	ch := j.ch

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

	b.mu.Unlock()
	b.finishJoin(topic, j)

	// When an external ReplayStore is configured, query it outside the lock.
	if store != nil {
		if lastEventID == "" {
			entries, err := store.Latest(context.Background(), topic, b.replayStoreLimit(topic))
			if err != nil {
				rse := &ReplayStoreError{Op: ReplayOpLatest, Topic: topic, Err: err}
				b.reportReplayStoreError(rse)
				if cfg.terminateOnReplayError {
					return b.failReplay(topic, ch, rse)
				}
			}
			for _, e := range entries {
				if e.ID != "" {
					replayMsgs = append(replayMsgs, injectSSEID(e.Msg, e.ID))
				} else {
					replayMsgs = append(replayMsgs, e.Msg)
				}
			}
		} else {
			entries, found, err := store.AfterID(context.Background(), topic, lastEventID, 0)
			if err != nil {
				// Partial entries and found are not trusted once the read
				// failed, so a failure-aware resume never reports a gap or a
				// successful reconnect for it.
				rse := &ReplayStoreError{Op: ReplayOpAfterID, Topic: topic, Err: err}
				b.reportReplayStoreError(rse)
				if cfg.terminateOnReplayError {
					return b.failReplay(topic, ch, rse)
				}
			}
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

	// Handle replay gap: fire callbacks, optionally send control event and snapshot.
	if gapDetected {
		// Fire registered gap callbacks regardless of strategy.
		for _, fn := range gapCallbacks {
			go fn(subInfo, lastEventID)
		}
		// For non-silent strategies, send control event and apply the strategy.
		if gapStrategy != GapSilent {
			b.trySend(ch, replayGapControlEvent(lastEventID))
			if gapStrategy == GapFallbackToSnapshot && gapSnapshotFn != nil {
				if snap := gapSnapshotFn(); snap != "" {
					b.trySend(ch, snap)
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
		if b.trySend(ch, bundled.String()) {
			replayDelivered = len(replayMsgs)
		} else {
			replayDropped = len(replayMsgs)
		}
	} else {
		for _, msg := range replayMsgs {
			if b.trySend(ch, msg) {
				replayDelivered++
			} else {
				replayDropped++
			}
		}
	}

	// Populate replay delivery stats and fire reconnect callbacks.
	if isReconnect {
		reconnectInfo.ReplayDelivered = replayDelivered
		reconnectInfo.ReplayDropped = replayDropped

		// Send reconnected control event with replay stats.
		b.trySend(ch, reconnectedControlEvent(replayDelivered, replayDropped))

		for _, fn := range reconnectCallbacks {
			go fn(reconnectInfo)
		}
		// Emit truncation control event when replay messages were dropped.
		if replayDropped > 0 {
			b.trySend(ch, replayTruncatedControlEvent(replayDelivered, replayDropped))
		}
	}

	return ch, b.unsubscriber(topic, ch)
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
//   - [SubWithDisconnectOnLoss]: covers LIVE delivery only. Replay is
//     bounded by the buffer and reported through tavern-replay-truncated;
//     it never terminates the subscription. Cannot be combined
//     with [SubWithRate].
//   - [SubWithTerminateOnReplayError]: a failed [ReplayStore] read ends the
//     subscription with [TerminationReplayFailure] instead of being treated
//     as an empty replay or a Last-Event-ID gap.
//   - [SubWithStatus]: records the terminal cause.
func (b *SSEBroker) SubscribeFromIDWith(topic, lastEventID string, opts ...SubscribeOption) (msgs <-chan string, unsubscribe func()) {
	cfg := buildSubscribeConfig(opts)
	cfg.rejectLossyCombinations()
	return b.subscribeFromIDInternal(topic, lastEventID, cfg)
}
