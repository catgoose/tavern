package tavern

import (
	"sync"
	"sync/atomic"
)

// TerminationReason says why a subscription ended.
type TerminationReason uint8

const (
	// TerminationNone means the subscription is still active.
	TerminationNone TerminationReason = iota
	// TerminationUnsubscribe means the caller's unsubscribe function ended it.
	TerminationUnsubscribe
	// TerminationDisconnect means [SSEBroker.Disconnect] removed it.
	TerminationDisconnect
	// TerminationQueueLoss means a [SubWithDisconnectOnLoss] subscription
	// could not queue a live message.
	TerminationQueueLoss
	// TerminationEvicted means slow-subscriber eviction or the adaptive
	// backpressure disconnect tier removed it.
	TerminationEvicted
	// TerminationBrokerClosed means [SSEBroker.Close] ended it, or the
	// broker was already closed when it was requested.
	TerminationBrokerClosed
	// TerminationReplayFailure means the [ReplayStore] failed while
	// replaying for a [SubWithTerminateOnReplayError] subscription. Err holds
	// the *[ReplayStoreError].
	TerminationReplayFailure
	// TerminationRejected means admission control refused the subscription.
	TerminationRejected
)

var terminationNames = [...]string{
	TerminationNone:          "active",
	TerminationUnsubscribe:   "unsubscribe",
	TerminationDisconnect:    "disconnect",
	TerminationQueueLoss:     "queue-loss",
	TerminationEvicted:       "evicted",
	TerminationBrokerClosed:  "broker-closed",
	TerminationReplayFailure: "replay-failure",
	TerminationRejected:      "rejected",
}

func (r TerminationReason) String() string {
	if int(r) < len(terminationNames) {
		return terminationNames[r]
	}
	return "unknown"
}

// Termination describes how a subscription ended.
type Termination struct {
	Reason TerminationReason
	// Topic is the topic whose subscription ended. For a multi-topic
	// subscription it is the topic that ended the aggregate, and empty when
	// the aggregate's own unsubscribe ended it.
	Topic string
	// Err is the cause for TerminationReplayFailure; nil otherwise.
	Err error
}

// SubscriptionStatus records the first terminal cause of one subscription.
// Create it with [NewSubscriptionStatus] and attach it with [SubWithStatus].
//
// The status is set before the subscription's data channel closes, so a
// reader that sees the channel closed can read the cause immediately. It
// does not depend on the data queue: it is readable while the queue is full,
// while the consumer is blocked elsewhere, and after the broker has dropped
// the subscription. Later teardown never overwrites the first cause.
type SubscriptionStatus struct {
	mu       sync.Mutex
	attached bool
	done     chan struct{}
	end      Termination
	endSeq   uint64 // orders ends across statuses; see lastEnded
}

// endCounter numbers terminations so lastEnded can tell which of several
// subscriptions ended last.
var endCounter atomic.Uint64

// NewSubscriptionStatus returns a status for a single subscription.
func NewSubscriptionStatus() *SubscriptionStatus {
	return &SubscriptionStatus{done: make(chan struct{})}
}

// Done returns a channel that is closed once the subscription has ended.
func (s *SubscriptionStatus) Done() <-chan struct{} {
	return s.done
}

// Termination returns how the subscription ended. Reason is
// TerminationNone while it is active.
func (s *SubscriptionStatus) Termination() Termination {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

// attach claims s for one subscription. Reusing a status would make its
// cause ambiguous, so it panics.
func (s *SubscriptionStatus) attach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached {
		panic("tavern: SubscriptionStatus is already attached to a subscription")
	}
	s.attached = true
}

// finish records t unless a cause is already recorded.
func (s *SubscriptionStatus) finish(t Termination) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end.Reason != TerminationNone {
		return
	}
	s.end = t
	s.endSeq = endCounter.Add(1)
	close(s.done)
}

// lastEnded returns the termination of the status that ended last, or false
// if none has ended.
func lastEnded(statuses []*SubscriptionStatus) (Termination, bool) {
	var last Termination
	var lastSeq uint64
	for _, s := range statuses {
		s.mu.Lock()
		if s.endSeq > lastSeq {
			last, lastSeq = s.end, s.endSeq
		}
		s.mu.Unlock()
	}
	return last, lastSeq > 0
}
