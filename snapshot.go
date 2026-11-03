package tavern

// SubscribeWithSnapshot subscribes to a topic and immediately sends the
// result of snapshotFn as the first message before any live publishes.
// The snapshot function runs while the subscription is being registered,
// ensuring no messages are missed between the snapshot and live stream.
// If snapshotFn returns an empty string, no snapshot is sent.
//
// This eliminates the dual-render pattern where page handlers and publishers
// independently render the same initial state.
func (b *SSEBroker) SubscribeWithSnapshot(topic string, snapshotFn func() string) (msgs <-chan string, unsubscribe func()) {
	// Compute snapshot outside lock to avoid deadlock if snapshotFn accesses broker.
	return b.subscribeLive(topic, subSpec{preload: snapshotFn()}, true)
}
