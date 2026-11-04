package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The transport page serves one topic two ways: SSEHandler with a finite
// connection lifetime and a reconnect delay, and StreamSSE with a wire
// heartbeat and a custom writer. The broker also sends keepalive comments
// to every subscriber. Comments never reach the page's EventSource, so the
// page captures the raw wire of each endpoint from a real local
// connection. Write failures are shown with a controlled writer that fails
// on purpose; that is a synthetic failure, not a socket or proxy timeout.
const (
	wrTopic     = "ops:wire"
	wrKeepalive = time.Second
	wrHeartbeat = 700 * time.Millisecond
	wrLifetime  = 3 * time.Second
	wrRetry     = 1500 * time.Millisecond
	wrTimeout   = 2 * time.Second
	wrCapture   = 3500 * time.Millisecond
	wrTickRetry = 2500 // ms, sent with each tick
)

type wireLab struct {
	o         *ops
	broker    *tavern.SSEBroker
	handler   http.Handler // SSEHandler
	custom    http.Handler // StreamSSE
	seq       atomic.Int64
	capturing atomic.Bool
	log       obsLog
}

func newWireLab(o *ops) *wireLab {
	l := &wireLab{o: o, broker: o.broker(tavern.WithBufferSize(16), tavern.WithKeepalive(wrKeepalive))}
	l.handler = l.broker.SSEHandler(wrTopic,
		tavern.WithMaxConnectionDuration(wrLifetime),
		tavern.WithReconnectDelay(wrRetry),
		tavern.WithWriteTimeout(wrTimeout),
		tavern.WithWriteErrorHandler(func(e *tavern.WriteError) { l.log.add("SSEHandler write failed at %s: %v", e.Op, e.Err) }),
		tavern.WithSSEWriter(func(w http.ResponseWriter, msg string) error {
			if _, err := io.WriteString(w, msg); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		}),
	)
	l.custom = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch, unsub := l.broker.Subscribe(wrTopic)
		if ch == nil {
			http.Error(w, "Too many connections", http.StatusServiceUnavailable)
			return
		}
		defer unsub()
		err := tavern.StreamSSE(r.Context(), w, ch, func(m string) string { return m },
			tavern.WithStreamHeartbeat(wrHeartbeat),
			tavern.WithStreamWriteTimeout(wrTimeout),
			tavern.WithStreamWriteErrors(),
			tavern.WithStreamWriter(func(w http.ResponseWriter, msg string) error {
				if _, err := io.WriteString(w, ": via custom writer\n"+msg); err != nil {
					return err
				}
				return http.NewResponseController(w).Flush()
			}),
		)
		if err != nil {
			l.log.add("StreamSSE ended: %v", err)
		}
	})
	return l
}

func (l *wireLab) tick() {
	n := l.seq.Add(1)
	// The frame carries its own reconnect delay (SSEMessage.WithRetry).
	l.broker.Publish(wrTopic, tavern.NewSSEMessage("tick", fmt.Sprintf("tick %d at %s", n, time.Now().Format("15:04:05.000"))).WithRetry(wrTickRetry).String())
}

// capture reads an endpoint's raw response over a real local connection for
// wrCapture, publishing a tick halfway. One capture at a time.
func (l *wireLab) capture(ctx context.Context, which string) (string, error) {
	if !l.capturing.CompareAndSwap(false, true) {
		return "", errors.New("a capture is already running")
	}
	defer l.capturing.Store(false)
	h := l.handler
	if which == "custom" {
		h = l.custom
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(ctx, wrCapture)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := srv.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	go func() {
		if sleepCtx(ctx, wrCapture/3) {
			l.tick()
		}
	}()
	start := time.Now()
	var b strings.Builder
	buf := make([]byte, 4096)
	for b.Len() < 64*1024 {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			fmt.Fprintf(&b, "[+%4dms]\n%s", time.Since(start).Milliseconds(), buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintf(&b, "[+%4dms] the server ended the response\n", time.Since(start).Milliseconds())
			} else {
				fmt.Fprintf(&b, "[+%4dms] capture stopped\n", time.Since(start).Milliseconds())
			}
			break
		}
	}
	return b.String(), nil
}

// failingWriter is a ResponseWriter that fails on purpose: its writes or
// flushes return an error, and it accepts write deadlines without
// enforcing them.
type failingWriter struct {
	header http.Header
	op     string
	wrote  int
}

var errSynthetic = errors.New("synthetic failure")

func (f *failingWriter) Header() http.Header { return f.header }
func (f *failingWriter) WriteHeader(int)     {}
func (f *failingWriter) Write(p []byte) (int, error) {
	if f.op == "write" {
		return 0, errSynthetic
	}
	f.wrote += len(p)
	return len(p), nil
}
func (f *failingWriter) FlushError() error {
	if f.op == "flush" {
		return errSynthetic
	}
	return nil
}
func (f *failingWriter) SetWriteDeadline(time.Time) error {
	if f.op == "deadline" {
		return errSynthetic
	}
	return nil
}

// fail streams one message to a failingWriter and reports the WriteError
// that ends the stream: the step that failed and its cause.
func (l *wireLab) fail(op string) string {
	ch := make(chan string, 1)
	ch <- tavern.NewSSEMessage("tick", "one message").String()
	ctx, cancel := context.WithTimeout(l.o.ctx, time.Second)
	defer cancel()
	err := tavern.StreamSSE(ctx, &failingWriter{header: http.Header{}, op: op}, ch, func(m string) string { return m },
		tavern.WithStreamWriteTimeout(wrTimeout), tavern.WithStreamWriteErrors())
	var we *tavern.WriteError
	if !errors.As(err, &we) {
		return fmt.Sprintf("no write error (%v)", err)
	}
	s := fmt.Sprintf("synthetic %s failure: WriteError at %s, cause %q", op, we.Op, errors.Unwrap(we))
	l.log.add("%s", s)
	return s
}

func (a *App) registerWire() {
	l := a.ops.wire
	a.mux.HandleFunc("GET /showcase/ops/wire", func(w http.ResponseWriter, _ *http.Request) {
		a.renderOpsPage(w, wirePage, map[string]any{"Title": "Transport lifetimes and write failures", "Family": "wire"})
	})
	a.mux.Handle("GET /showcase/ops/wire/sse", l.handler)
	a.mux.Handle("GET /showcase/ops/wire/custom", l.custom)
	a.mux.HandleFunc("POST /showcase/ops/wire/tick", func(w http.ResponseWriter, _ *http.Request) {
		l.tick()
		w.WriteHeader(http.StatusNoContent)
	})
	text := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, template.HTMLEscapeString(s))
	}
	a.mux.HandleFunc("POST /showcase/ops/wire/capture", func(w http.ResponseWriter, r *http.Request) {
		which := r.URL.Query().Get("which")
		if which != "sse" && which != "custom" {
			http.Error(w, "Unknown endpoint", http.StatusBadRequest)
			return
		}
		out, err := l.capture(r.Context(), which)
		if err != nil {
			out = err.Error()
		}
		text(w, out)
	})
	a.mux.HandleFunc("POST /showcase/ops/wire/fail", func(w http.ResponseWriter, r *http.Request) {
		op := r.URL.Query().Get("op")
		if op != "write" && op != "flush" && op != "deadline" {
			http.Error(w, "Unknown step", http.StatusBadRequest)
			return
		}
		text(w, l.fail(op))
	})
	a.mux.HandleFunc("GET /showcase/ops/wire/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"subscriptions": l.broker.SubscriberCount(), "ticks": l.seq.Load(), "observations": l.log.list()})
	})
}

var wirePage = deliveryPage("wire", `{{define "main"}}
<p class="note">One topic, served by SSEHandler (each connection ends after about 3 s with a <code>retry: 1500</code>, and the browser reconnects) and by StreamSSE (a heartbeat comment every 700 ms, frames written by a custom writer). The broker sends a keepalive comment to every subscriber each second. Comments are not visible to an EventSource, so the capture buttons read each endpoint's raw response over a real local connection for 3.5 s, publishing a tick partway through; each tick carries <code>retry: 2500</code> of its own. Both endpoints are also served at <code>/showcase/ops/wire/sse</code> and <code>/showcase/ops/wire/custom</code>.</p>
<p>
  <button hx-post="/showcase/ops/wire/tick" hx-swap="none">Publish a tick</button>
  <button hx-post="/showcase/ops/wire/capture?which=sse" hx-target="#wire-capture">Capture SSEHandler's wire</button>
  <button hx-post="/showcase/ops/wire/capture?which=custom" hx-target="#wire-capture">Capture StreamSSE's wire</button>
</p>
<pre id="wire-capture"></pre>
<h2>Write failures</h2>
<p class="note">These run StreamSSE against a writer that fails on purpose at one step, and show the WriteError it reports. They are synthetic; a real timeout needs a client that stops reading, which the Go tests use.</p>
<p>
  <button hx-post="/showcase/ops/wire/fail?op=write" hx-target="#wire-failure">Fail the write</button>
  <button hx-post="/showcase/ops/wire/fail?op=flush" hx-target="#wire-failure">Fail the flush</button>
  <button hx-post="/showcase/ops/wire/fail?op=deadline" hx-target="#wire-failure">Fail setting the deadline</button>
</p>
<p><output id="wire-failure"></output></p>
{{end}}`)
