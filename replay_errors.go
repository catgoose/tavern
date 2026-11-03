package tavern

import (
	"context"
	"fmt"
)

// ReplayStoreOp names a [ReplayStore] operation.
type ReplayStoreOp uint8

const (
	ReplayOpAppend ReplayStoreOp = iota + 1
	ReplayOpAfterID
	ReplayOpLatest
	ReplayOpDeleteTopic
	ReplayOpSetMaxEntries
)

var replayOpNames = [...]string{
	ReplayOpAppend:        "append",
	ReplayOpAfterID:       "after-id",
	ReplayOpLatest:        "latest",
	ReplayOpDeleteTopic:   "delete-topic",
	ReplayOpSetMaxEntries: "set-max-entries",
}

func (op ReplayStoreOp) String() string {
	if op > 0 && int(op) < len(replayOpNames) {
		return replayOpNames[op]
	}
	return "unknown"
}

// ReplayStoreError reports a failed [ReplayStore] operation. It wraps the
// store's error, so errors.Is and errors.As see the original cause.
type ReplayStoreError struct {
	Op    ReplayStoreOp
	Topic string
	Err   error
}

func (e *ReplayStoreError) Error() string {
	return fmt.Sprintf("tavern: replay store %s for topic %q: %v", e.Op, e.Topic, e.Err)
}

func (e *ReplayStoreError) Unwrap() error { return e.Err }

// WithReplayStoreErrorHandler registers fn to receive every failed
// [ReplayStore] operation. fn runs synchronously on the goroutine that
// called the store (a publisher, subscriber or policy call), after the
// failure and outside broker locks, so it must return quickly. Failures are
// also logged when [WithLogger] is set and counted in
// [BrokerStats].ReplayStoreErrors.
//
// Append runs before the live fan-out. A failed Append does not stop that
// best-effort delivery attempt, but the message is not retained for replay,
// and the failure says nothing about which subscribers or browsers received
// it. The broker does not retry.
func WithReplayStoreErrorHandler(fn func(*ReplayStoreError)) BrokerOption {
	return func(b *SSEBroker) {
		b.onReplayStoreError = fn
	}
}

// reportReplayStoreError records a failed store operation. The caller must
// not hold b.mu.
func (b *SSEBroker) reportReplayStoreError(rse *ReplayStoreError) {
	b.replayStoreErrs.Add(1)
	if b.logger != nil {
		b.logger.Error("replay store failed", "op", rse.Op.String(), "topic", rse.Topic, "err", rse.Err)
	}
	if b.onReplayStoreError != nil {
		b.onReplayStoreError(rse)
	}
}

func (b *SSEBroker) appendReplay(store ReplayStore, topic string, entry ReplayEntry) {
	if err := store.Append(context.Background(), topic, entry); err != nil {
		b.reportReplayStoreError(&ReplayStoreError{Op: ReplayOpAppend, Topic: topic, Err: err})
	}
}
