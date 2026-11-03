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
	writer       SSEWriterFunc // nil = default writer
	snapshot     func() string
	heartbeat    time.Duration
	writeTimeout time.Duration
	returnErrors bool
}

// WithStreamWriter overrides the default SSE frame writer used by [StreamSSE].
// The default writer writes each frame and flushes it. Override to integrate
// with libraries like htmx-go or to add custom SSE formatting. A nil function
// selects the default writer.
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

// WithStreamWriteTimeout bounds each frame write (snapshot, value and
// heartbeat), including its flush, to d. See [WithWriteTimeout] for how the
// deadline behaves. If the ResponseWriter cannot take deadlines, StreamSSE
// returns a [WriteError] matching [http.ErrNotSupported] before writing
// anything. A zero or negative duration disables the bound (the default).
func WithStreamWriteTimeout(d time.Duration) StreamSSEOption {
	return func(c *streamSSEConfig) {
		c.writeTimeout = d
	}
}

// WithStreamWriteErrors makes StreamSSE return the [WriteError] that ended
// the stream instead of nil. Context cancellation and channel close still
// return nil.
func WithStreamWriteErrors() StreamSSEOption {
	return func(c *streamSSEConfig) {
		c.returnErrors = true
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
// StreamSSE returns an error if w cannot flush (directly or through Unwrap),
// if encode is nil, or if a configured write timeout cannot be honored. On
// normal termination — context cancellation, channel close, or write failure
// — it returns nil; [WithStreamWriteErrors] reports write failures instead.
func StreamSSE[T any](
	ctx context.Context,
	w http.ResponseWriter,
	ch <-chan T,
	encode func(T) string,
	opts ...StreamSSEOption,
) error {
	if !canFlush(w) {
		return fmt.Errorf("tavern: StreamSSE requires http.ResponseWriter to implement http.Flusher")
	}
	if encode == nil {
		return fmt.Errorf("tavern: StreamSSE requires a non-nil encode function")
	}

	var cfg streamSSEConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	fw := newFrameWriter(w, cfg.writer, cfg.writeTimeout)
	if err := fw.prepare(); err != nil {
		return err
	}
	defer fw.releaseOnCancel(ctx)()
	writeFailed := func(err error) error {
		if cfg.returnErrors {
			return err
		}
		return nil
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if cfg.snapshot != nil {
		if frame := cfg.snapshot(); frame != "" {
			if err := fw.frame(frame); err != nil {
				return writeFailed(err)
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
			if err := fw.frame(frame); err != nil {
				return writeFailed(err)
			}
		case <-heartbeatC:
			if err := fw.frame(": keepalive\n\n"); err != nil {
				return writeFailed(err)
			}
		}
	}
}
