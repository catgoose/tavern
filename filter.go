package tavern

// FilterPredicate is a function that returns true if the message should be
// delivered to the subscriber. It runs synchronously in the publish goroutine
// for every message on the topic, so implementations must be fast and
// non-blocking. Messages rejected by the predicate are silently skipped and
// do not count toward drop counts or backpressure tiers.
type FilterPredicate func(msg string) bool

// SubscribeWithFilter registers a subscriber with a predicate filter for the
// given topic. Only messages for which the predicate returns true are delivered
// to the subscriber's channel. Non-matching messages are silently skipped and
// do not count toward drop counts or backpressure.
//
// The predicate runs in the publish goroutine — keep it fast.
func (b *SSEBroker) SubscribeWithFilter(topic string, predicate FilterPredicate) (msgs <-chan string, unsubscribe func()) {
	return b.subscribeLive(topic, subSpec{filter: predicate}, false)
}

// SubscribeScopedWithFilter registers a scoped subscriber with a predicate
// filter. Only messages published via [SSEBroker.PublishTo] with a matching
// scope AND passing the predicate are delivered. Non-matching messages are
// silently skipped.
func (b *SSEBroker) SubscribeScopedWithFilter(topic, scope string, predicate FilterPredicate) (msgs <-chan string, unsubscribe func()) {
	return b.subscribeLive(topic, subSpec{scope: scope, filter: predicate}, false)
}
