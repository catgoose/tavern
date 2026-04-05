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

// initBackend is a no-op placeholder called from NewSSEBroker. The actual
// integration happens inline: Subscribe/Unsubscribe call
// backendSubscribe/backendUnsubscribe when the first/last subscriber
// joins/leaves, and the Publish methods call backendPublish after local
// fan-out.
func (b *SSEBroker) initBackend() {}

// backendSubscribe registers the broker with the backend for the given topic
// and starts a goroutine that forwards incoming envelopes to local
// subscribers. It is called when the first local subscriber joins a topic
// and a backend is configured.
func (b *SSEBroker) backendSubscribe(topic string) {
	if b.backend == nil {
		return
	}
	ch, err := b.backend.Subscribe(context.Background(), topic)
	if err != nil {
		if b.logger != nil {
			b.logger.Error("backend subscribe failed", "topic", topic, "err", err)
		}
		return
	}
	go b.backendFanIn(topic, ch)
}

// backendUnsubscribe removes the broker's subscription from the backend for
// the given topic. It is called when the last local subscriber leaves a topic
// and a backend is configured.
func (b *SSEBroker) backendUnsubscribe(topic string) {
	if b.backend == nil {
		return
	}
	if err := b.backend.Unsubscribe(topic); err != nil {
		if b.logger != nil {
			b.logger.Error("backend unsubscribe failed", "topic", topic, "err", err)
		}
	}
}

// backendPublish forwards a message to the backend for cross-instance
// delivery. Scoped messages include the scope in the envelope.
func (b *SSEBroker) backendPublish(topic, msg, scope string) {
	if b.backend == nil {
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
		return
	}
	channels := make([]chan string, 0, len(subscribers))
	for ch := range subscribers {
		channels = append(channels, ch)
	}
	b.mu.RUnlock()
	b.publishToChannels(topic, channels, msg)
}

// dispatchScoped sends msg to scoped subscribers matching the given scope
// without applying middleware or firing After hooks.
func (b *SSEBroker) dispatchScoped(topic, scope, msg string) {
	b.mu.RLock()
	scopedSubs, exists := b.scopedTopics[topic]
	if !exists || len(scopedSubs) == 0 {
		b.mu.RUnlock()
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
}
