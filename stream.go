package tavern

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// StreamSSEOption configures [StreamSSE] behavior.
type StreamSSEOption func(*streamSSEConfig)

type streamSSEConfig struct {
	writer    SSEWriterFunc
	snapshot  func() string
	heartbeat time.Duration
}

// WithStreamWriter overrides the default SSE frame writer used by [StreamSSE].
// The default writer calls fmt.Fprint followed by http.Flusher.Flush for each
// frame. Override to integrate with libraries like htmx-go or to add custom
// SSE formatting.
func WithStreamWriter(fn SSEWriterFunc) StreamSSEOption {
	return func(c *streamSSEConfig) {
		c.writer = fn
	}
}

// WithStreamSnapshot registers a snapshot function that is called once, after
// SSE headers are written and before any channel values are streamed. The
// returned string is written verbatim as the initial frame. Return an empty
// string to skip the snapshot without disabling streaming.
//
// Use this to deliver server-rendered initial state to new subscribers.
func WithStreamSnapshot(fn func() string) StreamSSEOption {
	return func(c *streamSSEConfig) {
		c.snapshot = fn
	}
}

// WithStreamHeartbeat enables periodic SSE comment heartbeats while streaming.
// If interval is greater than zero, a ": keepalive\n\n" comment is written and
// flushed every interval to keep intermediaries (proxies, browsers) from
// closing idle connections. A zero or negative interval disables heartbeats.
//
// Tavern's broker-level keepalive (set via WithKeepalive on the broker) emits
// comments to all subscribers; WithStreamHeartbeat is the per-connection
// equivalent for handlers built directly on StreamSSE.
func WithStreamHeartbeat(interval time.Duration) StreamSSEOption {
	return func(c *streamSSEConfig) {
		c.heartbeat = interval
	}
}

// StreamSSE prepares an SSE response on w and streams frames from ch until
// ctx is cancelled or ch is closed. encode converts each channel value into
// the SSE frame to write; returning an empty string skips the value without
// terminating the stream.
//
// StreamSSE sits between raw subscription channels and the higher-level
// [SSEBroker.SSEHandler]. It handles the mechanical parts of an SSE response —
// standard headers, http.Flusher verification, context cancellation, optional
// snapshot delivery, and optional heartbeats — while leaving the choice of
// subscription API, filtering, and message-to-frame conversion at the call
// site. It does not subscribe on your behalf.
//
// Compose with any subscription API:
//
//	ch, unsub := broker.SubscribeScoped("orders", userID)
//	defer unsub()
//	return tavern.StreamSSE(r.Context(), w, ch, func(s string) string {
//	    return s
//	})
//
// For multiplexed channels that carry [TopicMessage], the encoder typically
// wraps each value with the topic as the event type:
//
//	ch, unsub := broker.SubscribeMulti("orders", "invoices")
//	defer unsub()
//	return tavern.StreamSSE(r.Context(), w, ch, func(tm tavern.TopicMessage) string {
//	    return tavern.NewSSEMessage(tm.Topic, tm.Data).String()
//	})
//
// StreamSSE returns an error if w does not implement [http.Flusher] or if
// encode is nil. On normal termination — context cancellation, channel close,
// or write failure — it returns nil.
func StreamSSE[T any](
	ctx context.Context,
	w http.ResponseWriter,
	ch <-chan T,
	encode func(T) string,
	opts ...StreamSSEOption,
) error {
	if _, ok := w.(http.Flusher); !ok {
		return fmt.Errorf("tavern: StreamSSE requires http.ResponseWriter to implement http.Flusher")
	}
	if encode == nil {
		return fmt.Errorf("tavern: StreamSSE requires a non-nil encode function")
	}

	cfg := streamSSEConfig{writer: defaultSSEWriter}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.writer == nil {
		cfg.writer = defaultSSEWriter
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if cfg.snapshot != nil {
		if frame := cfg.snapshot(); frame != "" {
			if err := cfg.writer(w, frame); err != nil {
				return nil
			}
		}
	}

	var heartbeatC <-chan time.Time
	if cfg.heartbeat > 0 {
		t := time.NewTicker(cfg.heartbeat)
		defer t.Stop()
		heartbeatC = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-ch:
			if !ok {
				return nil
			}
			frame := encode(v)
			if frame == "" {
				continue
			}
			if err := cfg.writer(w, frame); err != nil {
				return nil
			}
		case <-heartbeatC:
			if err := cfg.writer(w, ": keepalive\n\n"); err != nil {
				return nil
			}
		}
	}
}
