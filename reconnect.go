package tavern

import (
	"fmt"
	"time"
)

// ReconnectInfo provides context about a subscriber reconnection, including
// the gap duration and number of missed messages. It is passed to callbacks
// registered with [SSEBroker.OnReconnect].
type ReconnectInfo struct {
	// Topic is the topic the subscriber reconnected to.
	Topic string
	// SubscriberID is the caller-provided identifier (empty if not set via metadata).
	SubscriberID string
	// LastEventID is the Last-Event-ID sent by the client.
	LastEventID string
	// Gap is the time elapsed since the LastEventID was published. Zero if the
	// ID was not found in the replay log (e.g., it has rolled out).
	Gap time.Duration
	// MissedCount is the number of messages published after LastEventID that
	// the subscriber missed. Zero if the ID was not found in the replay log.
	MissedCount int
	// ReplayDelivered is the number of replay messages successfully enqueued
	// to the subscriber channel. This may be less than MissedCount if the
	// subscriber buffer was too small to hold all replay messages.
	ReplayDelivered int
	// ReplayDropped is the number of replay messages that could not be
	// enqueued because the subscriber buffer was full.
	ReplayDropped int
	// SendToSubscriber sends a message directly to this subscriber's channel.
	// The message is delivered as-is (raw SSE text). If the subscriber's buffer
	// is full the message is dropped silently.
	SendToSubscriber func(msg string)
}

// ReconnectCallback is invoked when a subscriber reconnects with a
// Last-Event-ID header. It fires on ALL reconnections regardless of whether
// the replay log can satisfy the request.
type ReconnectCallback func(info ReconnectInfo)

// OnReconnect registers a callback that fires when a subscriber reconnects
// with a Last-Event-ID header for the given topic. Unlike [SSEBroker.OnReplayGap],
// which only fires when the replay log cannot satisfy the request, OnReconnect
// fires on every reconnection. The callback runs in its own goroutine and does
// not block the subscription. Multiple callbacks per topic are allowed.
// Calling this on a closed broker is a no-op.
func (b *SSEBroker) OnReconnect(topic string, fn ReconnectCallback) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.onReconnect[topic] = append(b.onReconnect[topic], fn)
}

// SetBundleOnReconnect configures whether replay messages should be bundled
// into a single SSE write when a subscriber reconnects with a Last-Event-ID
// for the given topic. Bundling reduces DOM swap churn on the client by
// delivering all missed messages as one write.
func (b *SSEBroker) SetBundleOnReconnect(topic string, bundle bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if bundle {
		b.bundleOnReconnect[topic] = true
	} else {
		delete(b.bundleOnReconnect, topic)
	}
}

// reconnectedControlEvent returns the wire-format SSE control event that
// notifies clients a reconnection was detected.
func reconnectedControlEvent() string {
	return NewSSEMessage("tavern-reconnected", "").String()
}

// replayTruncatedControlEvent returns an SSE control event indicating that
// some replay messages were dropped due to subscriber buffer limits.
func replayTruncatedControlEvent(delivered, dropped int) string {
	return NewSSEMessage("tavern-replay-truncated",
		fmt.Sprintf("delivered:%d dropped:%d", delivered, dropped)).String()
}

// buildReconnectInfo constructs a ReconnectInfo from the replay log state.
// It must be called while holding b.mu. The returned info includes gap
// duration and missed count computed from the replay log.
func (b *SSEBroker) buildReconnectInfo(topic, lastEventID string, ch chan string) ReconnectInfo {
	info := ReconnectInfo{
		Topic:       topic,
		LastEventID: lastEventID,
		SendToSubscriber: func(msg string) {
			select {
			case ch <- msg:
			default:
			}
		},
	}

	// Set SubscriberID from metadata if available.
	if meta, ok := b.subscriberMeta[ch]; ok {
		info.SubscriberID = meta.ID
	}

	// Compute gap duration and missed count from the replay log.
	log := b.replayLog[topic]
	for i, entry := range log {
		if entry.ID == lastEventID {
			if !entry.PublishedAt.IsZero() {
				info.Gap = time.Since(entry.PublishedAt)
			}
			info.MissedCount = len(log) - i - 1
			break
		}
	}

	return info
}
