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
// A failed Append does not undo live delivery: the message reached current
// subscribers but was not retained for replay. The broker does not retry.
func WithReplayStoreErrorHandler(fn func(*ReplayStoreError)) BrokerOption {
	return func(b *SSEBroker) {
		b.onReplayStoreError = fn
	}
}

// replayStoreFailed records a failed store operation and returns the error
// reported for it. The caller must not hold b.mu.
func (b *SSEBroker) replayStoreFailed(op ReplayStoreOp, topic string, err error) *ReplayStoreError {
	rse := &ReplayStoreError{Op: op, Topic: topic, Err: err}
	b.replayStoreErrs.Add(1)
	if b.logger != nil {
		b.logger.Error("replay store failed", "op", op.String(), "topic", topic, "err", err)
	}
	if b.onReplayStoreError != nil {
		b.onReplayStoreError(rse)
	}
	return rse
}

func (b *SSEBroker) appendReplay(store ReplayStore, topic string, entry ReplayEntry) {
	if err := store.Append(context.Background(), topic, entry); err != nil {
		b.replayStoreFailed(ReplayOpAppend, topic, err)
	}
}
