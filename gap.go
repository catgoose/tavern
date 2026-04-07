package tavern

// GapStrategy determines how the broker responds when a subscriber reconnects
// with a Last-Event-ID that is no longer in the replay log (i.e., the log has
// rolled over and the requested ID is gone). Configure per-topic via
// [SSEBroker.SetReplayGapPolicy].
type GapStrategy int

const (
	// GapSilent is the default strategy. When a gap is detected, no replay
	// occurs and the subscriber receives only live messages going forward.
	// This preserves backwards compatibility with the existing behaviour.
	GapSilent GapStrategy = iota

	// GapFallbackToSnapshot uses the configured SnapshotFunc to generate a
	// full-state snapshot and delivers it to the subscriber before live
	// messages begin. This ensures the client can rebuild its state even
	// when the replay log has rolled over.
	GapFallbackToSnapshot
)

// ReplayGapCallback is invoked when a replay gap is detected for a subscriber.
// It receives the subscriber's info and the Last-Event-ID that could not be
// found in the replay log.
type ReplayGapCallback func(sub *SubscriberInfo, lastEventID string)

// OnReplayGap registers a callback that fires when a subscriber reconnects
// with a Last-Event-ID that is no longer present in the replay log for the
// given topic. The callback runs in its own goroutine and does not block the
// subscription. Multiple callbacks per topic are allowed and all will fire.
// Like [SSEBroker.SetReplayGapPolicy], gap callbacks are only meaningful
// when the topic uses ID-backed replay (see SetReplayGapPolicy for details).
// Calling this on a closed broker is a no-op.
func (b *SSEBroker) OnReplayGap(topic string, fn ReplayGapCallback) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.onReplayGap[topic] = append(b.onReplayGap[topic], fn)
}

// SetReplayGapPolicy configures the gap strategy and optional snapshot
// function for the given topic. When a subscriber reconnects with a
// Last-Event-ID that has rolled out of the replay log:
//
//   - [GapSilent]: no special action (default, backwards compatible).
//   - [GapFallbackToSnapshot]: call snapshotFn and deliver the result as
//     the first message to the subscriber, preceded by a
//     "event: tavern-replay-gap" control event.
//
// The snapshotFn parameter is only used with [GapFallbackToSnapshot] and
// may be nil for other strategies.
//
// Gap detection requires ID-backed replay state: the topic must receive
// messages via [SSEBroker.PublishWithID] (or variants like PublishWithTTL)
// so that a replay log with event IDs exists. Without ID-backed publishes,
// subscribers never receive event IDs and Last-Event-ID reconnection is
// not meaningful. Calling SetReplayGapPolicy on a topic that only uses
// plain [SSEBroker.Publish] has no effect at runtime.
//
// If a [*slog.Logger] is configured via [WithLogger], a warning is logged
// when this method is called for a topic that has no replay log entries
// and no external [ReplayStore].
func (b *SSEBroker) SetReplayGapPolicy(topic string, strategy GapStrategy, snapshotFn func() string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if b.logger != nil && b.replayStore == nil && len(b.replayLog[topic]) == 0 {
		if _, hasSize := b.replaySize[topic]; !hasSize {
			b.logger.Warn("SetReplayGapPolicy called on topic with no ID-backed replay state; gap detection requires PublishWithID",
				"topic", topic,
			)
		}
	}
	b.replayGapStrategy[topic] = strategy
	if snapshotFn != nil {
		b.replayGapSnapshot[topic] = snapshotFn
	} else {
		delete(b.replayGapSnapshot, topic)
	}
}

// replayGapControlEvent returns the wire-format SSE control event that
// notifies clients a replay gap was detected.
func replayGapControlEvent(lastEventID string) string {
	return NewSSEMessage("tavern-replay-gap", lastEventID).String()
}
