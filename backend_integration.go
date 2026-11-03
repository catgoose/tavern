package tavern

import (
	"context"

	"github.com/catgoose/tavern/backend"
)

// WithBackend configures the broker to use a cross-process fan-out backend.
// When set, every Publish also forwards the message to the backend, and the
// broker automatically subscribes to the backend when the first local
// subscriber joins a topic and unsubscribes when the last local subscriber
// leaves.
//
// Messages arriving from the backend are dispatched directly to local
// subscriber channels — they skip middleware and After hooks to avoid
// duplicate side-effects across instances.
func WithBackend(b backend.Backend) BrokerOption {
	return func(br *SSEBroker) {
		br.backend = b
	}
}

// initBackend sets up health-aware reconnection if the backend implements
// [backend.HealthAwareBackend]. On reconnect, the broker re-subscribes to
// all topics that currently have local subscribers.
func (b *SSEBroker) initBackend() {
	if b.backend == nil {
		return
	}
	if hab, ok := b.backend.(backend.HealthAwareBackend); ok {
		hab.OnReconnect(func() {
			b.resubscribeAll()
		})
	}
}

// resubscribeAll re-subscribes to the backend for every topic that currently
// has at least one local subscriber. This is called after a backend reconnect.
func (b *SSEBroker) resubscribeAll() {
	b.mu.RLock()
	seen := make(map[string]struct{}, len(b.topics)+len(b.scopedTopics))
	for topic, subs := range b.topics {
		if len(subs) > 0 {
			seen[topic] = struct{}{}
		}
	}
	for topic, subs := range b.scopedTopics {
		if len(subs) > 0 {
			seen[topic] = struct{}{}
		}
	}
	b.mu.RUnlock()

	for topic := range seen {
		b.resubscribeBackend(topic)
	}
}

// resubscribeBackend renews the backend subscription for topic if it still
// has local subscribers, serialized with first/last transitions.
func (b *SSEBroker) resubscribeBackend(topic string) {
	st := b.acquireBackendTopic(topic)
	defer b.releaseBackendTopic(topic, st)
	st.mu.Lock()
	defer st.mu.Unlock()
	if b.localSubscribers(topic) {
		st.active = b.backendSubscribe(topic, st)
	}
}

// backendSubscribe registers the broker with the backend for the given topic
// and reports whether it succeeded. A fan-in goroutine forwarding envelopes
// to local subscribers starts unless the backend returned the channel that
// is already being fanned in. The caller holds st.mu.
func (b *SSEBroker) backendSubscribe(topic string, st *backendTopic) bool {
	ch, err := b.backend.Subscribe(context.Background(), topic)
	if err != nil {
		if b.logger != nil {
			b.logger.Error("backend subscribe failed", "topic", topic, "err", err)
		}
		return false
	}
	if ch != st.ch {
		st.ch = ch
		go b.backendFanIn(topic, ch)
	}
	return true
}

// backendUnsubscribe removes the broker's subscription from the backend for
// the given topic. It is called from syncBackend once the topic has no local
// subscribers.
func (b *SSEBroker) backendUnsubscribe(topic string) {
	if err := b.backend.Unsubscribe(topic); err != nil {
		if b.logger != nil {
			b.logger.Error("backend unsubscribe failed", "topic", topic, "err", err)
		}
	}
}

// backendPublish forwards a message to the backend for cross-instance
// delivery. Scoped messages include the scope in the envelope. If the
// backend implements [backend.HealthAwareBackend] and is unhealthy, the
// publish is silently skipped.
func (b *SSEBroker) backendPublish(topic, msg, scope string) {
	if b.backend == nil {
		return
	}
	if hab, ok := b.backend.(backend.HealthAwareBackend); ok && !hab.Healthy() {
		return
	}
	env := backend.MessageEnvelope{
		Topic: topic,
		Data:  msg,
		Scope: scope,
	}
	if err := b.backend.Publish(context.Background(), env); err != nil {
		if b.logger != nil {
			b.logger.Error("backend publish failed", "topic", topic, "err", err)
		}
	}
}

// backendPublishWithMeta forwards a message with TTL and ID metadata.
func (b *SSEBroker) backendPublishWithMeta(topic, msg, scope string, ttlMs int64, id string) {
	if b.backend == nil {
		return
	}
	if hab, ok := b.backend.(backend.HealthAwareBackend); ok && !hab.Healthy() {
		return
	}
	env := backend.MessageEnvelope{
		Topic: topic,
		Data:  msg,
		Scope: scope,
		TTL:   ttlMs,
		ID:    id,
	}
	if err := b.backend.Publish(context.Background(), env); err != nil {
		if b.logger != nil {
			b.logger.Error("backend publish failed", "topic", topic, "err", err)
		}
	}
}

// backendFanIn reads envelopes from the backend channel and dispatches them
// directly to local subscribers. Messages skip middleware and After hooks
// because the originating instance already applied those.
func (b *SSEBroker) backendFanIn(topic string, ch <-chan backend.MessageEnvelope) {
	for env := range ch {
		if env.Scope != "" {
			b.dispatchScoped(env.Topic, env.Scope, env.Data)
		} else {
			b.dispatchDirect(env.Topic, env.Data)
		}
	}
}

// dispatchDirect sends msg to all unscoped subscribers of the topic without
// applying middleware or firing After hooks. Used for messages arriving from
// the backend.
func (b *SSEBroker) dispatchDirect(topic, msg string) {
	b.mu.RLock()
	subscribers, exists := b.topics[topic]
	if !exists || len(subscribers) == 0 {
		b.mu.RUnlock()
		b.dispatchToGlobSubscribers(topic, "", msg)
		return
	}
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.RUnlock()
	b.publishToChannels(topic, channels, msg)
	b.dispatchToGlobSubscribers(topic, "", msg)
}

// dispatchScoped sends msg to scoped subscribers matching the given scope
// without applying middleware or firing After hooks.
func (b *SSEBroker) dispatchScoped(topic, scope, msg string) {
	b.mu.RLock()
	scopedSubs, exists := b.scopedTopics[topic]
	if !exists || len(scopedSubs) == 0 {
		b.mu.RUnlock()
		b.dispatchToGlobSubscribers(topic, scope, msg)
		return
	}
	channels := make([]chan string, 0, len(scopedSubs))
	for ch, sub := range scopedSubs {
		if sub.scope == scope {
			channels = append(channels, ch)
		}
	}
	b.mu.RUnlock()
	if len(channels) > 0 {
		b.publishToChannels(topic, channels, msg)
	}
	b.dispatchToGlobSubscribers(topic, scope, msg)
}
