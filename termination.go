package tavern

import "sync"

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
	// onEnd, set before the status is shared, receives the cause as soon as
	// it is recorded. It can run under broker and channel locks, so it must
	// only touch its own leaf state.
	onEnd func(Termination)
}

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

func (s *SubscriptionStatus) isAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached
}

// finish records t unless a cause is already recorded.
func (s *SubscriptionStatus) finish(t Termination) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.end.Reason != TerminationNone {
		s.mu.Unlock()
		return
	}
	s.end = t
	close(s.done)
	onEnd := s.onEnd
	s.mu.Unlock()
	if onEnd != nil {
		onEnd(t)
	}
}

// aggregateCause picks a multi-topic subscription's cause from its inner
// subscriptions at the moment each one ends, so the choice follows the order
// the inner subscriptions actually ended rather than when fan-in workers run.
// A required aggregate takes the first inner cause; otherwise it takes the
// cause of the inner subscription that ends last.
type aggregateCause struct {
	status   *SubscriptionStatus
	required bool
	mu       sync.Mutex
	pending  int // inner subscriptions not yet ended, plus one until built
	last     Termination
}

func newAggregateCause(status *SubscriptionStatus, required bool) *aggregateCause {
	return &aggregateCause{status: status, required: required, pending: 1}
}

// track returns a status for one inner subscription that reports its end to
// the aggregate.
func (a *aggregateCause) track() *SubscriptionStatus {
	a.mu.Lock()
	a.pending++
	a.mu.Unlock()
	st := NewSubscriptionStatus()
	st.onEnd = a.ended
	return st
}

func (a *aggregateCause) ended(t Termination) {
	if a.required {
		a.status.finish(t)
		return
	}
	a.mu.Lock()
	a.last = t
	a.pending--
	allEnded := a.pending == 0
	a.mu.Unlock()
	if allEnded {
		a.status.finish(t)
	}
}

// built marks construction complete. An aggregate whose inner subscriptions
// all ended during construction ends with the last of them, or as rejected
// when none was created.
func (a *aggregateCause) built() {
	a.mu.Lock()
	a.pending--
	allEnded := a.pending == 0
	last := a.last
	a.mu.Unlock()
	if !allEnded {
		return
	}
	if last.Reason == TerminationNone {
		last = Termination{Reason: TerminationRejected}
	}
	a.status.finish(last)
}
