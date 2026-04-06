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

type sseHandler struct {
	broker          *SSEBroker
	topic           string
	writer          SSEWriterFunc
	maxConnDuration time.Duration
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
			writeMaxDurationClose(w)
			return
		}
	}
}

// writeMaxDurationClose writes the retry hint and comment before closing a
// connection that has reached its maximum duration.
func writeMaxDurationClose(w http.ResponseWriter) {
	fmt.Fprint(w, "retry: 1000\n\n")
	fmt.Fprint(w, ": max connection duration reached\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
