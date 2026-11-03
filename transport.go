package tavern

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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

// frameWriter performs every write of one SSE response: snapshot, live,
// heartbeat, replay and closing frames. With a timeout, each write and flush
// runs under its own write deadline, cleared again once the frame is out so
// idle periods between frames are not limited.
type frameWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	writer  SSEWriterFunc // nil = default write and flush
	timeout time.Duration
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
// unblocks a write stuck on a deadline-capable connection. The returned stop
// function must be called before the handler returns. Without a timeout it
// does nothing: writers that cannot take deadlines cannot be interrupted.
func (fw *frameWriter) releaseOnCancel(ctx context.Context) (stop func()) {
	if fw.timeout <= 0 {
		return func() {}
	}
	fired := make(chan struct{})
	stopAfter := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = fw.rc.SetWriteDeadline(time.Now())
	})
	return func() {
		// Wait for a running callback so it cannot touch the response after
		// the handler returns.
		if !stopAfter() {
			<-fired
		}
	}
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
	if err := fw.rc.SetWriteDeadline(time.Now().Add(fw.timeout)); err != nil {
		return &WriteError{Op: WriteOpDeadline, Err: err}
	}
	if err := write(); err != nil {
		return err
	}
	if err := fw.rc.SetWriteDeadline(time.Time{}); err != nil {
		return &WriteError{Op: WriteOpDeadline, Err: err}
	}
	return nil
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
