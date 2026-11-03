package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// Scenario describes one bounded load run.
type Scenario struct {
	Name string `json:"name"`
	// Transport is "broker" (in-process subscribers) or "http" (real SSE
	// clients over loopback HTTP/1.1).
	Transport    string `json:"transport"`
	Subscribers  int    `json:"subscribers"`   // readers, spread evenly over Topics
	Topics       int    `json:"topics"`        // distinct keys
	Messages     int    `json:"messages"`      // published per topic
	PayloadBytes int    `json:"payload_bytes"` // message size
	Buffer       int    `json:"buffer"`        // queue length per subscriber
	Replay       int    `json:"replay"`        // replay entries kept per topic; 0 = none
	Slow         int    `json:"slow"`          // extra subscribers that never read
	LossAware    bool   `json:"loss_aware"`    // end subscribers on queue loss
	// ReconnectBurst readers reconnect at once after publishing, resuming
	// from the first message they received.
	ReconnectBurst int           `json:"reconnect_burst"`
	Timeout        time.Duration `json:"timeout"` // bound on waiting for delivery
}

// Report holds what one run measured. Queue counts describe Tavern's queues;
// they say nothing about what a browser rendered.
type Report struct {
	Scenario Scenario `json:"scenario"`
	Env      Env      `json:"env"`

	PublishSeconds float64 `json:"publish_seconds"`
	// FanoutAttempts is messages × subscribers on each topic when published.
	FanoutAttempts int64 `json:"fanout_attempts"`
	QueueDrops     int64 `json:"queue_drops"`
	// Terminations counts how subscriptions ended, by reason.
	Terminations map[string]int `json:"terminations"`

	// Received counts messages read by the reading subscribers; for http
	// it counts SSE frames parsed by the clients.
	Received     int64   `json:"received"`
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`
	LatencyMaxMs float64 `json:"latency_max_ms"`

	ReconnectReplayed int64 `json:"reconnect_replayed"`
	ReconnectGaps     int64 `json:"reconnect_gaps"`

	HeapBaselineMB  float64 `json:"heap_baseline_mb"`
	HeapPublishedMB float64 `json:"heap_published_mb"` // right after publishing, while readers drain
	HeapAfterMB     float64 `json:"heap_after_mb"`  // after every subscriber left
	GoroutinesStart int     `json:"goroutines_start"`
	GoroutinesEnd   int     `json:"goroutines_end"`
	// WorstCaseQueueMB estimates queue memory if every queued message were
	// a separate copy: (subscribers + slow) × buffer × payload. Published
	// strings are shared by every queue, so measured heap is usually far
	// lower.
	WorstCaseQueueMB float64 `json:"worst_case_queue_mb"`
}

// Env records where a run happened.
type Env struct {
	Go   string `json:"go"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
	CPUs int    `json:"cpus"`
	CPU  string `json:"cpu"`
}

func currentEnv() Env {
	return Env{Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), CPU: cpuModel()}
}

func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "model name"); ok {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), ":"))
		}
	}
	return ""
}

// ReconnectBurstSubscriptions is the number of extra subscriptions the
// reconnect burst creates.
func (s Scenario) ReconnectBurstSubscriptions() int {
	if s.Replay > 0 {
		return s.ReconnectBurst
	}
	return 0
}

func (s Scenario) validate() error {
	switch {
	case s.Transport != "broker" && s.Transport != "http":
		return fmt.Errorf("transport must be broker or http")
	case s.Subscribers < 1 || s.Topics < 1 || s.Messages < 1 || s.Buffer < 1:
		return fmt.Errorf("subscribers, topics, messages and buffer must be positive")
	case s.PayloadBytes < 24:
		return fmt.Errorf("payload must be at least 24 bytes to carry a timestamp")
	case s.ReconnectBurst > s.Subscribers:
		return fmt.Errorf("reconnect burst cannot exceed subscribers")
	case s.Timeout <= 0:
		return fmt.Errorf("timeout must be positive")
	}
	return nil
}

func mb(b uint64) float64 { return float64(b) / (1 << 20) }

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return mb(m.HeapAlloc)
}

// payload returns a message carrying its publish time, padded to size with
// a shared filler.
func payload(filler string, size int) string {
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	return ts + "|" + filler[:size-len(ts)-1]
}

func latencyOf(msg string, now time.Time) (time.Duration, bool) {
	ts, _, ok := strings.Cut(msg, "|")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0, false
	}
	return now.Sub(time.Unix(0, n)), true
}

// latencies collects per-message delivery latency.
type latencies struct {
	mu  sync.Mutex
	all []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.all = append(l.all, d)
	l.mu.Unlock()
}

func (l *latencies) fill(r *Report) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.all) == 0 {
		return
	}
	slices.Sort(l.all)
	at := func(q float64) float64 {
		return float64(l.all[int(q*float64(len(l.all)-1))]) / float64(time.Millisecond)
	}
	r.LatencyP50Ms, r.LatencyP99Ms = at(0.50), at(0.99)
	r.LatencyMaxMs = float64(l.all[len(l.all)-1]) / float64(time.Millisecond)
}

func topicName(i int) string { return "key-" + strconv.Itoa(i) }

// Run executes one scenario and tears everything down before returning.
func Run(s Scenario) (Report, error) {
	if err := s.validate(); err != nil {
		return Report{}, err
	}
	r := Report{Scenario: s, Env: currentEnv(), Terminations: map[string]int{}}
	r.WorstCaseQueueMB = mb(uint64(s.Subscribers+s.Slow) * uint64(s.Buffer) * uint64(s.PayloadBytes))
	r.GoroutinesStart = runtime.NumGoroutine()
	r.HeapBaselineMB = heapMB()

	var err error
	if s.Transport == "broker" {
		err = runBroker(s, &r)
	} else {
		err = runHTTP(s, &r)
	}
	r.HeapAfterMB = heapMB()
	r.GoroutinesEnd = settledGoroutines(r.GoroutinesStart)
	return r, err
}

// settledGoroutines waits briefly for exiting goroutines to finish.
func settledGoroutines(start int) int {
	n := runtime.NumGoroutine()
	for i := 0; i < 200 && n > start; i++ {
		time.Sleep(5 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// publishAll publishes Messages to every topic, round-robin, and returns
// the fan-out attempts.
func publishAll(s Scenario, b *tavern.SSEBroker, frame func(string) string) int64 {
	filler := strings.Repeat("x", s.PayloadBytes)
	var attempts int64
	for m := 0; m < s.Messages; m++ {
		for t := 0; t < s.Topics; t++ {
			topic := topicName(t)
			attempts += int64(len(b.Subscribers(topic)))
			msg := frame(payload(filler, s.PayloadBytes))
			if s.Replay > 0 {
				b.PublishWithID(topic, strconv.Itoa(m+1), msg)
			} else {
				b.Publish(topic, msg)
			}
		}
	}
	return attempts
}

func newBroker(s Scenario, gaps *atomic.Int64) *tavern.SSEBroker {
	b := tavern.NewSSEBroker(tavern.WithBufferSize(s.Buffer))
	for t := 0; t < s.Topics; t++ {
		topic := topicName(t)
		if s.Replay > 0 {
			b.SetReplayPolicy(topic, s.Replay)
		}
		b.OnReplayGap(topic, func(*tavern.SubscriberInfo, string) { gaps.Add(1) })
	}
	return b
}

func subscribeOpts(s Scenario, st *tavern.SubscriptionStatus) []tavern.SubscribeOption {
	opts := []tavern.SubscribeOption{tavern.SubWithStatus(st)}
	if s.LossAware {
		opts = append(opts, tavern.SubWithDisconnectOnLoss())
	}
	return opts
}

func runBroker(s Scenario, r *Report) error {
	var gaps atomic.Int64
	b := newBroker(s, &gaps)
	type sub struct {
		topic  string
		ch     <-chan string
		unsub  func()
		status *tavern.SubscriptionStatus
	}
	var subs []*sub
	add := func(i int) *sub {
		st := tavern.NewSubscriptionStatus()
		topic := topicName(i % s.Topics)
		ch, unsub := b.SubscribeWith(topic, subscribeOpts(s, st)...)
		v := &sub{topic: topic, ch: ch, unsub: unsub, status: st}
		subs = append(subs, v)
		return v
	}
	readers := make([]*sub, s.Subscribers)
	for i := range readers {
		readers[i] = add(i)
	}
	for i := 0; i < s.Slow; i++ {
		add(i)
	}

	var lat latencies
	var received atomic.Int64
	var wg sync.WaitGroup
	for _, rd := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for msg := range rd.ch {
				if d, ok := latencyOf(msg, time.Now()); ok {
					lat.add(d)
				}
				received.Add(1)
			}
		}()
	}

	t0 := time.Now()
	r.FanoutAttempts = publishAll(s, b, func(m string) string { return m })
	r.PublishSeconds = time.Since(t0).Seconds()
	r.HeapPublishedMB = heapMB()

	waitDrained(&received, int64(s.Subscribers)*int64(s.Messages), s.Timeout, func() bool {
		for _, rd := range readers {
			if len(rd.ch) > 0 {
				return false
			}
		}
		return true
	})

	if s.ReconnectBurst > 0 && s.Replay > 0 {
		var burst sync.WaitGroup
		var replayed atomic.Int64
		statuses := make([]*tavern.SubscriptionStatus, s.ReconnectBurst)
		for i, rd := range readers[:s.ReconnectBurst] {
			rd.unsub()
			statuses[i] = tavern.NewSubscriptionStatus()
			burst.Add(1)
			go func() {
				defer burst.Done()
				ch, unsub := b.SubscribeFromIDWith(rd.topic, "1", tavern.SubWithStatus(statuses[i]))
				defer unsub()
				for {
					select {
					case msg := <-ch:
						if msg == "" || strings.Contains(msg, "tavern-reconnected") {
							return
						}
						if !strings.HasPrefix(msg, "event: tavern-") {
							replayed.Add(1)
						}
					case <-time.After(s.Timeout):
						return
					}
				}
			}()
		}
		burst.Wait()
		r.ReconnectReplayed = replayed.Load()
		for _, st := range statuses {
			r.Terminations[st.Termination().Reason.String()]++
		}
	}

	r.QueueDrops = b.PublishDrops()
	for _, v := range subs {
		v.unsub()
	}
	wg.Wait()
	for _, v := range subs {
		r.Terminations[v.status.Termination().Reason.String()]++
	}
	r.Received = received.Load()
	r.ReconnectGaps = gaps.Load()
	lat.fill(r)
	b.Close()
	return nil
}

// waitDrained waits until expected messages arrived, or until nothing is
// queued and the count has stopped moving (messages were dropped), or the
// timeout.
func waitDrained(received *atomic.Int64, expected int64, timeout time.Duration, empty func() bool) {
	deadline := time.Now().Add(timeout)
	last, stable := int64(-1), 0
	for received.Load() < expected && time.Now().Before(deadline) {
		n := received.Load()
		if n == last && empty() {
			stable++
			if stable >= 20 {
				return
			}
		} else {
			stable = 0
		}
		last = n
		time.Sleep(time.Millisecond)
	}
}

// runHTTP serves each topic over SSE and connects real clients over
// loopback. Slow clients connect with raw TCP and never read.
func runHTTP(s Scenario, r *Report) error {
	var gaps atomic.Int64
	b := newBroker(s, &gaps)
	var statusMu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		st := tavern.NewSubscriptionStatus()
		topic := strings.TrimPrefix(req.URL.Path, "/")
		var ch <-chan string
		var unsub func()
		if id := req.Header.Get("Last-Event-ID"); id != "" {
			ch, unsub = b.SubscribeFromIDWith(topic, id, subscribeOpts(s, st)...)
		} else {
			ch, unsub = b.SubscribeWith(topic, subscribeOpts(s, st)...)
		}
		if ch == nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = tavern.StreamSSE(req.Context(), w, ch, func(m string) string { return m },
			tavern.WithStreamSnapshot(func() string { return ": open\n\n" }),
			tavern.WithStreamWriteTimeout(2*time.Second))
		unsub()
		statusMu.Lock()
		r.Terminations[st.Termination().Reason.String()]++
		statusMu.Unlock()
	})
	srv := httptest.NewServer(handler)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: s.Subscribers + s.ReconnectBurst}}

	var lat latencies
	var received atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	open := make(chan error, s.Subscribers)
	for i := 0; i < s.Subscribers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readStream(ctx, client, srv.URL+"/"+topicName(i%s.Topics), "", open, func(data string) {
				if d, ok := latencyOf(data, time.Now()); ok {
					lat.add(d)
				}
				received.Add(1)
			})
		}()
	}
	var slowConns []net.Conn
	for i := 0; i < s.Slow; i++ {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			cancel()
			return err
		}
		fmt.Fprintf(conn, "GET /%s HTTP/1.1\r\nHost: capacity\r\n\r\n", topicName(i%s.Topics))
		slowConns = append(slowConns, conn)
	}
	for i := 0; i < s.Subscribers; i++ {
		if err := <-open; err != nil {
			cancel()
			return err
		}
	}
	waitSubscribers(b, s.Subscribers+s.Slow, s.Timeout)

	t0 := time.Now()
	r.FanoutAttempts = publishAll(s, b, func(m string) string { return "data: " + m + "\n\n" })
	r.PublishSeconds = time.Since(t0).Seconds()
	r.HeapPublishedMB = heapMB()

	// The server's queues are not visible to the clients, so a drop shows
	// up as the count settling below the expected total.
	waitDrained(&received, int64(s.Subscribers)*int64(s.Messages), s.Timeout, func() bool { return true })

	if s.ReconnectBurst > 0 && s.Replay > 0 {
		var replayed atomic.Int64
		var burst sync.WaitGroup
		burstOpen := make(chan error, s.ReconnectBurst)
		burstCtx, burstCancel := context.WithTimeout(context.Background(), s.Timeout)
		for i := 0; i < s.ReconnectBurst; i++ {
			burst.Add(1)
			go func() {
				defer burst.Done()
				readStream(burstCtx, client, srv.URL+"/"+topicName(i%s.Topics), "1", burstOpen, func(string) {
					replayed.Add(1)
				})
			}()
		}
		// The burst connections replay what the log still holds; stop
		// once that has had time to arrive.
		for i := 0; i < s.ReconnectBurst; i++ {
			if err := <-burstOpen; err != nil {
				burstCancel()
				burst.Wait()
				cancel()
				return err
			}
		}
		// Resuming after ID 1 replays the rest of the log, up to the queue
		// length, or nothing if ID 1 has rolled out of the log (a gap).
		var want int64
		if s.Replay >= s.Messages {
			want = int64(s.ReconnectBurst) * int64(min(s.Messages-1, s.Buffer))
		}
		for replayed.Load() < want && burstCtx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		burstCancel()
		burst.Wait()
		r.ReconnectReplayed = replayed.Load()
	}

	r.QueueDrops = b.PublishDrops()
	cancel()
	for _, c := range slowConns {
		_ = c.Close()
	}
	wg.Wait()
	// Let handlers see their clients leave before closing the broker, so
	// their subscriptions end as unsubscribes.
	waitSubscribers(b, 0, s.Timeout)
	b.Close()
	srv.Close()
	client.CloseIdleConnections()
	r.Received = received.Load()
	r.ReconnectGaps = gaps.Load()
	lat.fill(r)
	return nil
}

// waitSubscribers waits until exactly n subscriptions are registered.
func waitSubscribers(b *tavern.SSEBroker, n int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for b.SubscriberCount() != n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// readStream reads one SSE stream until ctx ends or the stream closes. It
// reports on open once response headers arrive (the server sends an
// ": open" comment first), and calls onData for every data frame that is not
// a named control event.
func readStream(ctx context.Context, client *http.Client, url, lastID string, open chan<- error, onData func(data string)) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, http.NoBody)
	if err != nil {
		open <- err
		return
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := client.Do(req)
	if err != nil {
		open <- err
		return
	}
	defer resp.Body.Close()
	open <- nil
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var event, data string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data != "" && event == "" {
				onData(data)
			}
			event, data = "", ""
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		}
	}
}
