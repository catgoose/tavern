package tavern

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"
)

// WriteOp names the step of an SSE response write that failed.
type WriteOp uint8

const (
	// WriteOpDeadline is setting or clearing the per-write deadline,
	// including when the ResponseWriter does not support deadlines.
	WriteOpDeadline WriteOp = iota + 1
	// WriteOpWrite is writing a frame, including a custom [SSEWriterFunc].
	WriteOpWrite
	// WriteOpFlush is flushing a frame written by the default writer.
	WriteOpFlush
)

var writeOpNames = [...]string{
	WriteOpDeadline: "deadline",
	WriteOpWrite:    "write",
	WriteOpFlush:    "flush",
}

func (op WriteOp) String() string {
	if op > 0 && int(op) < len(writeOpNames) {
		return writeOpNames[op]
	}
	return "unknown"
}

// WriteError reports a failed SSE response write. It wraps the underlying
// error, so errors.Is works with causes such as [http.ErrNotSupported] (the
// writer cannot set deadlines) and [os.ErrDeadlineExceeded] (a write timed
// out on a net/http connection).
//
// A successful write or flush only means the bytes were handed to the
// connection; it says nothing about what the browser received or applied.
type WriteError struct {
	Op  WriteOp
	Err error
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("tavern: SSE %s failed: %v", e.Op, e.Err)
}

func (e *WriteError) Unwrap() error { return e.Err }

// errWriteCanceled ends a protected stream whose request was cancelled. It
// is never reported: cancellation is a normal way for a stream to end.
var errWriteCanceled = errors.New("tavern: SSE stream canceled")

// frameWriter performs every write of one SSE response: snapshot, live,
// heartbeat, replay and closing frames. With a timeout, each write and flush
// runs under its own write deadline, cleared again once the frame is out so
// idle periods between frames are not limited.
type frameWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	writer  SSEWriterFunc // nil = default write and flush
	timeout time.Duration

	// mu serializes every deadline change with cancellation. Once
	// cancelled is set the deadline stays expired: no frame sets a new one
	// or clears it, and no new protected write starts. mu is never held
	// across a write or flush.
	mu        sync.Mutex
	ctx       context.Context
	cancelled bool
	cancelErr error // failure to expire the deadline on cancellation
}

func newFrameWriter(w http.ResponseWriter, writer SSEWriterFunc, timeout time.Duration) *frameWriter {
	return &frameWriter{w: w, rc: http.NewResponseController(w), writer: writer, timeout: timeout}
}

// prepare checks, before anything is written, that a configured timeout can
// be honored. It returns a WriteOpDeadline error otherwise.
func (fw *frameWriter) prepare() error {
	if fw.timeout <= 0 {
		return nil
	}
	if err := fw.rc.SetWriteDeadline(time.Time{}); err != nil {
		return &WriteError{Op: WriteOpDeadline, Err: err}
	}
	return nil
}

// releaseOnCancel makes cancellation of ctx expire the write deadline, which
// unblocks a write stuck on a deadline-capable connection, and stops later
// frames from writing. The returned stop function must be called before the
// handler returns; it reports a failure to expire the deadline. Without a
// timeout it does nothing: writers that cannot take deadlines cannot be
// interrupted.
func (fw *frameWriter) releaseOnCancel(ctx context.Context) (stop func() error) {
	if fw.timeout <= 0 {
		return func() error { return nil }
	}
	fw.mu.Lock()
	fw.ctx = ctx
	fw.mu.Unlock()
	fired := make(chan struct{})
	stopAfter := context.AfterFunc(ctx, func() {
		defer close(fired)
		fw.cancel()
	})
	return func() error {
		// Wait for a running callback so it cannot touch the response after
		// the handler returns.
		if !stopAfter() {
			<-fired
		}
		fw.mu.Lock()
		defer fw.mu.Unlock()
		if fw.cancelErr != nil {
			return &WriteError{Op: WriteOpDeadline, Err: fw.cancelErr}
		}
		return nil
	}
}

func (fw *frameWriter) cancel() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.cancelled {
		return
	}
	fw.cancelled = true
	// A connection that is already closed has nothing left to release.
	if err := fw.rc.SetWriteDeadline(time.Now()); err != nil && !errors.Is(err, net.ErrClosed) {
		fw.cancelErr = err
	}
}

// setDeadline changes the write deadline unless the stream was cancelled.
// The caller must not hold fw.mu.
func (fw *frameWriter) setDeadline(t time.Time) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.cancelled || fw.ctx != nil && fw.ctx.Err() != nil {
		return errWriteCanceled
	}
	if err := fw.rc.SetWriteDeadline(t); err != nil {
		return &WriteError{Op: WriteOpDeadline, Err: err}
	}
	return nil
}

// isCancelled reports whether the request was cancelled. It checks the
// context too: over HTTP/2 a client cancel resets the stream, and the write
// can fail before the cancellation callback runs.
func (fw *frameWriter) isCancelled() bool {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.cancelled || fw.ctx != nil && fw.ctx.Err() != nil
}

// frame writes msg with the configured writer.
func (fw *frameWriter) frame(msg string) error {
	return fw.protect(func() error {
		if fw.writer != nil {
			if err := fw.writer(fw.w, msg); err != nil {
				return &WriteError{Op: WriteOpWrite, Err: err}
			}
			return nil
		}
		return fw.writeAndFlush(msg)
	})
}

// raw writes msg with the default write and flush, bypassing a custom
// writer. Used for protocol frames such as the max-duration retry.
func (fw *frameWriter) raw(msg string) error {
	return fw.protect(func() error { return fw.writeAndFlush(msg) })
}

func (fw *frameWriter) writeAndFlush(msg string) error {
	if _, err := io.WriteString(fw.w, msg); err != nil {
		return &WriteError{Op: WriteOpWrite, Err: err}
	}
	// A writer that cannot flush is tolerated as before; the frame is
	// delivered when the server flushes its buffer.
	if err := fw.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return &WriteError{Op: WriteOpFlush, Err: err}
	}
	return nil
}

func (fw *frameWriter) protect(write func() error) error {
	if fw.timeout <= 0 {
		return write()
	}
	due := time.Now().Add(fw.timeout)
	if err := fw.setDeadline(due); err != nil {
		return err
	}
	if err := write(); err != nil {
		// A write that fails before its own deadline after cancellation was
		// cut short by it. net/http also cancels the request after a write
		// fails, so a failure at or past the deadline stays a timeout.
		if fw.isCancelled() && time.Now().Before(due) {
			return errWriteCanceled
		}
		return err
	}
	return fw.setDeadline(time.Time{})
}

// canFlush reports whether w, or a writer it wraps through Unwrap, can flush.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch t := w.(type) {
		case http.Flusher, interface{ FlushError() error }:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
		default:
			return false
		}
	}
}

// withJitter adds up to 10% random jitter to d.
func withJitter(d time.Duration) time.Duration {
	if j := int64(d / 10); j > 0 {
		return d + time.Duration(rand.Int64N(j))
	}
	return d
}

// maxDurationCloseFrame is the retry hint and comment written before closing
// a connection that reached its maximum duration. A zero or negative delay
// uses 1000ms.
func maxDurationCloseFrame(delay time.Duration) string {
	ms := delay.Milliseconds()
	if ms <= 0 {
		ms = 1000
	}
	return fmt.Sprintf("retry: %d\n\n: max connection duration reached\n\n", ms)
}
