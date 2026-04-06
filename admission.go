package tavern

// WithMaxSubscribers sets a global limit on the total number of concurrent
// subscribers across all topics. Default is 0 (unlimited). When the limit is
// reached, new Subscribe calls return a nil channel and nil unsubscribe
// function, and SSEHandler returns HTTP 503 Service Unavailable.
func WithMaxSubscribers(n int) BrokerOption {
	return func(b *SSEBroker) {
		b.maxSubs = n
	}
}

// WithMaxSubscribersPerTopic sets a per-topic limit on the number of
// concurrent subscribers. Default is 0 (unlimited). When the limit for a
// topic is reached, new Subscribe calls for that topic return a nil channel
// and nil unsubscribe function, and SSEHandler returns HTTP 503.
func WithMaxSubscribersPerTopic(n int) BrokerOption {
	return func(b *SSEBroker) {
		b.maxSubsPerTopic = n
	}
}

// WithAdmissionControl sets a custom admission function that is called for
// every new subscription attempt. The function receives the topic name and
// the current total subscriber count for that topic. It should return true
// to allow the subscription or false to deny it.
func WithAdmissionControl(fn func(topic string, currentCount int) bool) BrokerOption {
	return func(b *SSEBroker) {
		b.admissionFn = fn
	}
}

// admitSubscriber checks all admission constraints under the caller's lock.
// Returns true if the subscription should be allowed.
func (b *SSEBroker) admitSubscriber(topic string) bool {
	if b.maxSubs > 0 {
		total := 0
		for _, subs := range b.topics {
			total += len(subs)
		}
		for _, subs := range b.scopedTopics {
			total += len(subs)
		}
		if total >= b.maxSubs {
			return false
		}
	}
	if b.maxSubsPerTopic > 0 {
		topicCount := len(b.topics[topic]) + len(b.scopedTopics[topic])
		if topicCount >= b.maxSubsPerTopic {
			return false
		}
	}
	if b.admissionFn != nil {
		topicCount := len(b.topics[topic]) + len(b.scopedTopics[topic])
		if !b.admissionFn(topic, topicCount) {
			return false
		}
	}
	return true
}
