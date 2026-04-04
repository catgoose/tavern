package tavern

import (
	"fmt"
	"net/http"
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

type sseHandler struct {
	broker *SSEBroker
	topic  string
	writer SSEWriterFunc
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

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe with Last-Event-ID resumption.
	lastID := r.Header.Get("Last-Event-ID")
	ch, unsub := h.broker.SubscribeFromID(h.topic, lastID)
	defer unsub()

	// Stream loop.
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
		}
	}
}
