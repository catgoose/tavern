// Package backend defines the pluggable interface for cross-process fan-out
// in tavern. Backends transport messages between broker instances so that a
// publish on one process reaches subscribers connected to another.
package backend

import "context"

// MessageEnvelope is the unit of data exchanged between broker instances
// through a Backend.
type MessageEnvelope struct {
	// Topic is the pub/sub topic the message belongs to.
	Topic string `json:"topic"`
	// Data is the serialised message payload (typically SSE-formatted text).
	Data string `json:"data"`
	// Scope restricts delivery to scoped subscribers when non-empty.
	Scope string `json:"scope,omitempty"`
}

// Backend is the interface that cross-process fan-out implementations must
// satisfy. A Backend transports messages between independent broker instances
// so that a publish on instance A reaches subscribers connected to instance B.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Backend interface {
	// Publish sends an envelope to all other instances subscribed to the
	// same topic. The call should be non-blocking or return quickly.
	Publish(ctx context.Context, env MessageEnvelope) error

	// Subscribe registers interest in a topic and returns a channel that
	// receives envelopes published by other instances. The returned channel
	// is closed when the topic is unsubscribed or the backend is closed.
	Subscribe(ctx context.Context, topic string) (<-chan MessageEnvelope, error)

	// Unsubscribe removes a previously registered subscription for the
	// given topic. The channel returned by Subscribe will be closed.
	Unsubscribe(topic string) error

	// Close shuts down the backend and releases all resources. Any open
	// subscription channels are closed.
	Close() error
}
