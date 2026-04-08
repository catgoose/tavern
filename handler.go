package tavern

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

// SSEWriterFunc writes a message to the HTTP response. It is called for each
// message received from the subscriber channel. The default writer calls
// fmt.Fprint followed by Flush. Override with [WithSSEWriter] to use custom
// formatting (e.g., htmx-go).
type SSEWriterFunc func(w http.ResponseWriter, msg string) error

// SSEHandlerOption configures the SSE handler.
type SSEHandlerOption func(*sseHandler)

// WithSSEWriter overrides the default message writer. The provided function
// is called for each message and is responsible for writing to the response
// and flushing if needed. Use this to integrate with libraries like htmx-go
// or to add custom SSE formatting.
func WithSSEWriter(fn SSEWriterFunc) SSEHandlerOption {
	return func(h *sseHandler) {
		h.writer = fn
	}
}

// WithMaxConnectionDuration sets a maximum lifetime for SSE connections. After
// the configured duration (plus 0-10% random jitter to prevent thundering herd),
// the handler sends a retry: directive and an SSE comment, then closes the
// connection. The browser's EventSource will automatically reconnect with
// Last-Event-ID, providing seamless resumption.
//
// A zero or negative duration disables the limit.
func WithMaxConnectionDuration(d time.Duration) SSEHandlerOption {
	return func(h *sseHandler) {
		h.maxConnDuration = d
	}
}

// WithReconnectDelay sets the SSE retry: value (in milliseconds) sent when a
// connection is closed due to [WithMaxConnectionDuration]. The browser's
// EventSource uses this value to determine how long to wait before
// reconnecting. A zero or negative value defaults to 1000ms.
func WithReconnectDelay(delay time.Duration) SSEHandlerOption {
	return func(h *sseHandler) {
		h.reconnectDelay = delay
	}
}

type sseHandler struct {
	broker          *SSEBroker
	topic           string
	writer          SSEWriterFunc
	maxConnDuration time.Duration
	reconnectDelay  time.Duration
}

// SSEHandler returns an [http.Handler] that streams SSE messages for the
// given topic. It handles Content-Type headers, Last-Event-ID resumption
// via [SSEBroker.SubscribeFromID], and the streaming select loop.
//
// The default writer calls fmt.Fprint followed by http.Flusher.Flush for
// each message. Override with [WithSSEWriter] for custom formatting.
//
//	// Standard library
//	mux.Handle("/sse/dashboard", broker.SSEHandler("dashboard"))
//
//	// Echo
//	e.GET("/sse/dashboard", echo.WrapHandler(broker.SSEHandler("dashboard")))
//
//	// Custom writer (e.g., htmx-go)
//	mux.Handle("/sse", broker.SSEHandler("events",
//	    tavern.WithSSEWriter(func(w http.ResponseWriter, msg string) error {
//	        return htmx.WriteSSE(w, msg)
//	    }),
//	))
func (b *SSEBroker) SSEHandler(topic string, opts ...SSEHandlerOption) http.Handler {
	h := &sseHandler{
		broker: b,
		topic:  topic,
		writer: defaultSSEWriter,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func defaultSSEWriter(w http.ResponseWriter, msg string) error {
	if _, err := fmt.Fprint(w, msg); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// ServeHTTP handles an incoming SSE connection by setting the appropriate
// headers, subscribing to the handler's topic (with Last-Event-ID resumption
// if the header is present), and streaming messages until the client
// disconnects or the broker closes.
func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe with Last-Event-ID resumption.
	lastID := r.Header.Get("Last-Event-ID")
	ch, unsub := h.broker.SubscribeFromID(h.topic, lastID)
	if ch == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer unsub()

	// Stream loop.
	var maxDurC <-chan time.Time
	if h.maxConnDuration > 0 {
		actual := h.maxConnDuration + time.Duration(rand.Int64N(int64(h.maxConnDuration/10)))
		maxDurC = time.After(actual)
	}

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return // broker closed
			}
			if err := h.writer(w, msg); err != nil {
				return // client disconnected
			}
		case <-r.Context().Done():
			return // client disconnected
		case <-maxDurC:
			writeMaxDurationClose(w, h.reconnectDelay)
			return
		}
	}
}

// writeMaxDurationClose writes the retry hint and comment before closing a
// connection that has reached its maximum duration. When delay is zero or
// negative, the default of 1000ms is used.
func writeMaxDurationClose(w http.ResponseWriter, delay time.Duration) {
	ms := delay.Milliseconds()
	if ms <= 0 {
		ms = 1000
	}
	fmt.Fprintf(w, "retry: %d\n\n", ms)
	fmt.Fprint(w, ": max connection duration reached\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
