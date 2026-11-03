package tavern

import (
	"errors"
	"net/http"
	"time"
)

// SSEWriterFunc writes a message to the HTTP response. It is called for each
// message received from the subscriber channel. The default writer writes
// the message and flushes it. Override with [WithSSEWriter] to use custom
// formatting (e.g., htmx-go).
type SSEWriterFunc func(w http.ResponseWriter, msg string) error

// SSEHandlerOption configures the SSE handler.
type SSEHandlerOption func(*sseHandler)

// WithSSEWriter overrides the default message writer. The provided function
// is called for each message and is responsible for writing to the response
// and flushing if needed. Use this to integrate with libraries like htmx-go
// or to add custom SSE formatting. A nil function selects the default writer.
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

// WithWriteTimeout bounds each frame write, including its flush, to d. A
// frame that cannot be written in time ends the connection with a
// [WriteError] whose cause matches [os.ErrDeadlineExceeded]. Each deadline
// is cleared once its frame is written, so idle time between frames is not
// limited, and while it is set it replaces the server's WriteTimeout for the
// connection. Cancelling the request context expires the deadline, which
// releases a write blocked on a slow client, and no further frame is
// written. A write cut short by cancellation is not reported as an error.
//
// Deadlines require a ResponseWriter that supports them through
// [http.ResponseController], directly or via Unwrap. If it does not, the
// handler responds 500 before subscribing instead of streaming without the
// requested bound. A custom [WithSSEWriter] runs under the same deadline but
// is only interrupted if it writes to the connection. A zero or negative
// duration disables the bound (the default).
func WithWriteTimeout(d time.Duration) SSEHandlerOption {
	return func(h *sseHandler) {
		h.writeTimeout = d
	}
}

// WithWriteErrorHandler registers fn to receive the [WriteError] that ends a
// connection, including a missing deadline capability. fn runs synchronously
// on the handler goroutine before the handler unsubscribes and returns, so it
// should return quickly. Failures are also logged at debug level (error level
// for a missing capability) when the broker has a logger.
func WithWriteErrorHandler(fn func(*WriteError)) SSEHandlerOption {
	return func(h *sseHandler) {
		h.onWriteError = fn
	}
}

// handlerStream is the streaming configuration shared by the single-topic
// and group handlers.
type handlerStream struct {
	writer          SSEWriterFunc // nil = default writer
	maxConnDuration time.Duration
	reconnectDelay  time.Duration
	writeTimeout    time.Duration
	onWriteError    func(*WriteError)
}

type sseHandler struct {
	broker *SSEBroker
	topic  string
	handlerStream
}

// handlerStreamFrom applies handler options and returns the resulting
// configuration.
func handlerStreamFrom(opts []SSEHandlerOption) handlerStream {
	var h sseHandler
	for _, opt := range opts {
		opt(&h)
	}
	return h.handlerStream
}

// SSEHandler returns an [http.Handler] that streams SSE messages for the
// given topic. It handles Content-Type headers, Last-Event-ID resumption
// via [SSEBroker.SubscribeFromID], and the streaming select loop.
//
// The default writer writes each message and flushes it. Override with
// [WithSSEWriter] for custom formatting.
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
	return &sseHandler{broker: b, topic: topic, handlerStream: handlerStreamFrom(opts)}
}

// begin checks that the configured protection can be honored and sets the
// SSE headers. When it cannot, it responds 500 and returns nil, so no
// successful stream is started.
func (c *handlerStream) begin(b *SSEBroker, w http.ResponseWriter) *frameWriter {
	fw := newFrameWriter(w, c.writer, c.writeTimeout)
	if err := fw.prepare(); err != nil {
		c.report(b, err)
		http.Error(w, "SSE write deadlines are not supported by this response writer", http.StatusInternalServerError)
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return fw
}

// stopRelease stops cancellation handling and reports a failure to expire
// the write deadline on cancellation.
func (c *handlerStream) stopRelease(b *SSEBroker, stop func() error) {
	if err := stop(); err != nil {
		c.report(b, err)
	}
}

// report hands the error that ended a connection to the observer and logger.
// Cancellation is not reported.
func (c *handlerStream) report(b *SSEBroker, err error) {
	if errors.Is(err, errWriteCanceled) {
		return
	}
	var we *WriteError
	if !errors.As(err, &we) {
		we = &WriteError{Op: WriteOpWrite, Err: err}
	}
	if c.onWriteError != nil {
		c.onWriteError(we)
	}
	if b.logger != nil {
		if errors.Is(we, http.ErrNotSupported) {
			b.logger.Error("SSE write deadline unsupported", "err", we)
		} else {
			b.logger.Debug("SSE write failed", "op", we.Op.String(), "err", we.Err)
		}
	}
}

// maxDuration returns a channel that fires when the connection reaches its
// maximum duration, or nil when there is no limit.
func (c *handlerStream) maxDuration() (expired <-chan time.Time, stop func()) {
	if c.maxConnDuration <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(withJitter(c.maxConnDuration))
	return t.C, func() { t.Stop() }
}

// closeForMaxDuration writes the retry hint and comment that end a connection
// at its maximum duration.
func (c *handlerStream) closeForMaxDuration(b *SSEBroker, fw *frameWriter) {
	if err := fw.raw(maxDurationCloseFrame(c.reconnectDelay)); err != nil {
		c.report(b, err)
	}
}

// ServeHTTP handles an incoming SSE connection by setting the appropriate
// headers, subscribing to the handler's topic (with Last-Event-ID resumption
// if the header is present), and streaming messages until the client
// disconnects or the broker closes.
func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fw := h.begin(h.broker, w)
	if fw == nil {
		return
	}

	// Subscribe with Last-Event-ID resumption.
	lastID := r.Header.Get("Last-Event-ID")
	ch, unsub := h.broker.SubscribeFromID(h.topic, lastID)
	if ch == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer unsub()
	defer h.stopRelease(h.broker, fw.releaseOnCancel(r.Context()))

	maxDurC, stopMaxDur := h.maxDuration()
	defer stopMaxDur()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return // broker closed
			}
			if err := fw.frame(msg); err != nil {
				h.report(h.broker, err)
				return
			}
		case <-r.Context().Done():
			return // client disconnected
		case <-maxDurC:
			h.closeForMaxDuration(h.broker, fw)
			return
		}
	}
}
