package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tab is one open presence stream that the test can close.
type tab struct {
	events <-chan recEvent
	close  context.CancelFunc
}

func openTab(t *testing.T, tb *testBoard, as, conn string) tab {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/ops/presence/events?as="+as+"&conn="+conn, http.NoBody)
	events := make(chan recEvent, 64)
	go func() {
		defer close(events)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		readEvents(resp, events)
	}()
	return tab{events: events, close: cancel}
}

// readEvents parses an SSE response into events until it ends.
func readEvents(resp *http.Response, events chan<- recEvent) {
	scanner := bufio.NewScanner(resp.Body)
	var ev recEvent
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if ev.name != "" {
				ev.data = strings.Join(data, "\n")
				events <- ev
			}
			ev, data = recEvent{}, nil
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, "retry: "):
			events <- recEvent{name: "retry", data: strings.TrimPrefix(line, "retry: ")}
		}
	}
}

// present is the tracker's users and their tab counts.
func present(l *presenceLab) map[string]any {
	out := map[string]any{}
	for _, u := range l.tracker.List(prRoom) {
		out[u.UserID] = u.Metadata["tabs"]
	}
	return out
}

func TestOpsPresenceCountsTabs(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.presence
	a1 := openTab(t, tb, "alice", "a1")
	waitFor(t, a1.events, `id="people"`, time.Second) // the snapshot
	a2 := openTab(t, tb, "alice", "a2")
	b1 := openTab(t, tb, "bob", "b1")
	require.Eventually(t, func() bool {
		p := present(l)
		return p["alice"] == 2 && p["bob"] == 1
	}, time.Second, 5*time.Millisecond, "one entry per person, counting tabs")
	waitFor(t, b1.events, `data-user="alice" data-status="here" data-tabs="2"`, time.Second)

	a1.close()
	require.Eventually(t, func() bool { return present(l)["alice"] == 1 }, time.Second, 5*time.Millisecond, "closing one tab keeps the person")
	a2.close()
	require.Eventually(t, func() bool { _, ok := present(l)["alice"]; return !ok }, time.Second, 5*time.Millisecond)
	assert.True(t, l.log.has("Alice removed (left: last tab closed)"))
	ev := waitFor(t, b1.events, `data-user="bob"`, time.Second)
	for !strings.Contains(ev.data, `data-user="bob"`) || strings.Contains(ev.data, `data-user="alice"`) {
		ev = waitFor(t, b1.events, `data-user="bob"`, time.Second)
	}

	late := openTab(t, tb, "alice", "a3")
	snap := recNext(t, late.events)
	assert.Contains(t, snap.data, `data-user="bob"`, "a new tab starts from the current list")
	assert.NotContains(t, snap.data, `data-user="alice"`, "before its own join")
	waitFor(t, late.events, `data-user="alice"`, time.Second)
}

func TestOpsPresenceStaleAndHeartbeats(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.presence
	openTab(t, tb, "alice", "a1")
	openTab(t, tb, "alice", "a2")
	require.Eventually(t, func() bool { return present(l)["alice"] == 2 }, time.Second, 5*time.Millisecond)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/presence/heartbeats?conn=a1&on=0", nil))
	time.Sleep(prStale + 1500*time.Millisecond)
	assert.Equal(t, 2, present(l)["alice"], "the other tab's heartbeats keep Alice present")

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/presence/heartbeats?conn=a2&on=0", nil))
	require.Eventually(t, func() bool { _, ok := present(l)["alice"]; return !ok }, prStale+2*time.Second, 20*time.Millisecond, "with no heartbeats Alice goes stale")
	assert.True(t, l.log.has("Alice removed (stale: no heartbeat for 2 s)"))

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/presence/heartbeats?conn=a1&on=1", nil))
	require.Eventually(t, func() bool { return present(l)["alice"] == 2 }, 2*time.Second, 20*time.Millisecond, "a tab that resumes joins again")
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/ops/presence/heartbeats?conn=nobody&on=0", nil))
}

// TestOpsPresenceRendersStayCurrent makes many concurrent changes; every
// window's last render must match the tracker once they settle.
func TestOpsPresenceRendersStayCurrent(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.presence
	watcher := openTab(t, tb, "bob", "w")
	openTab(t, tb, "alice", "a1")
	require.Eventually(t, func() bool { return len(present(l)) == 2 }, time.Second, 5*time.Millisecond)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := []string{"alice", "bob"}[i%2]
			l.tracker.Update(prRoom, user, map[string]any{"status": prStatuses[i%3]})
		}()
	}
	wg.Wait()
	got := recUntilQuiet(watcher.events, 300*time.Millisecond)
	require.NotEmpty(t, got)
	assert.Equal(t, l.peopleFragment(), got[len(got)-1].data, "the last render is the current list")
	assert.Equal(t, http.StatusBadRequest, tb.post(t, "/showcase/ops/presence/status?conn=a1&value=asleep", nil))
}

func TestOpsPresenceRefusedTabIsNotPresent(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.presence
	openTab(t, tb, "alice", "same")
	require.Eventually(t, func() bool { return present(l)["alice"] == 1 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, http.StatusConflict, tb.status(t, "/showcase/ops/presence/events?as=alice&conn=same"))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, present(l)["alice"], "the refused tab was never counted")
	assert.Equal(t, http.StatusBadRequest, tb.status(t, "/showcase/ops/presence/events?as=mallory&conn=x"))
}

func connsStream(t *testing.T, tb *testBoard, as, conn string) <-chan recEvent {
	t.Helper()
	return sseStream(t, tb, "/showcase/ops/conns/events?as="+as+"&conn="+conn, "")
}

func TestOpsConnectionEventsAndIntrospection(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.conns
	x := connsStream(t, tb, "alice", "x")
	require.Eventually(t, func() bool { return l.broker.HasSubscribers(cnFeed) }, time.Second, 5*time.Millisecond)
	y := connsStream(t, tb, "bob", "y")
	ev := waitFor(t, x, `"event":"subscribe"`, time.Second)
	assert.Equal(t, "conn-event", ev.name)
	assert.JSONEq(t, `{"event":"subscribe","topic":"ops:feed","subscribers":2}`, ev.data)

	subs := l.broker.Subscribers(cnFeed)
	require.Len(t, subs, 2)
	ids := map[string]map[string]string{}
	for _, s := range subs {
		ids[s.ID] = s.Meta
	}
	assert.Equal(t, map[string]string{"persona": "alice", "page": "conns"}, ids["x"])
	assert.Equal(t, 4, l.broker.SubscriberCount(), "two windows, two subscriptions each")
	assert.Equal(t, map[string]int{cnFeed: 2, cnMeta: 2}, l.broker.TopicCounts())
	for _, e := range recUntilQuiet(x, 100*time.Millisecond) {
		assert.NotContains(t, e.data, `"topic":"ops:meta"`, "the event topic does not report itself")
	}

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/conns/burst", nil))
	waitFor(t, y, `data-seq="10"`, time.Second)
	m := l.broker.Metrics()
	assert.Equal(t, int64(2*cnBurst), m.TopicStats[cnFeed].Published, "one delivery per subscription per message")
	assert.Equal(t, 2, m.TopicStats[cnFeed].PeakSubscribers)
	snap := l.broker.Observability().Snapshot(l.broker)
	assert.Positive(t, snap.Topics[cnFeed].PublishLatency.Count)

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/conns/disconnect?conn=x", nil))
	require.Eventually(t, func() bool { return l.reg.owner("x") == nil }, time.Second, 5*time.Millisecond, "x's stream ends")
	gone := waitFor(t, y, `"event":"disconnect"`, time.Second)
	assert.JSONEq(t, `{"event":"disconnect","topic":"ops:feed","subscribers":1}`, gone.data)
	assert.Equal(t, map[string]int{cnFeed: 1, cnMeta: 1}, l.broker.TopicCounts(), "y keeps both subscriptions")
	assert.NotEmpty(t, l.broker.Observability().ConnectionDurations(cnFeed), "the closed connection's duration is recorded")
	connsStream(t, tb, "alice", "x")
	require.Eventually(t, func() bool { return len(l.broker.Subscribers(cnFeed)) == 2 }, time.Second, 5*time.Millisecond, "x reconnects with the same ID")
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/ops/conns/disconnect?conn=nobody", nil))
}

func (tb *testBoard) admit(t *testing.T, path string) string {
	t.Helper()
	resp, err := tb.srv.Client().Post(tb.srv.URL+"/showcase/ops/admit/"+path, "", http.NoBody)
	require.NoError(t, err)
	defer resp.Body.Close()
	b := new(strings.Builder)
	_, _ = bufio.NewReader(resp.Body).WriteTo(b)
	require.Equal(t, http.StatusOK, resp.StatusCode, b.String())
	return b.String()
}

func TestOpsAdmission(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.admit
	g := l.policies["global"].broker
	assert.Contains(t, tb.admit(t, "stream"), "200 OK", "room for a stream")
	for i, topic := range []string{"a", "b", "a"} {
		assert.Contains(t, tb.admit(t, "add?policy=global&topic=ops:admit:"+topic), "admitted; "+fmt.Sprint(i+1)+" held")
	}
	assert.Contains(t, tb.admit(t, "add?policy=global&topic=ops:admit:b"), "refused (at most 3 subscriptions on the broker); 3 held")
	assert.Equal(t, 3, g.SubscriberCount(), "the refusal changed nothing")
	assert.Contains(t, tb.admit(t, "stream"), "503 Service Unavailable", "SSEHandler refuses a stream while full")
	assert.Contains(t, tb.admit(t, "remove?policy=global"), "removed; 2 held")
	assert.Contains(t, tb.admit(t, "add?policy=global&topic=ops:admit:b"), "admitted; 3 held", "capacity returns")

	p := l.policies["topic"].broker
	tb.admit(t, "add?policy=topic&topic=ops:admit:a")
	tb.admit(t, "add?policy=topic&topic=ops:admit:a")
	assert.Contains(t, tb.admit(t, "add?policy=topic&topic=ops:admit:a"), "refused (at most 2 subscriptions per topic)")
	assert.Contains(t, tb.admit(t, "add?policy=topic&topic=ops:admit:b"), "admitted", "another topic has room")
	out := tb.admit(t, "multi?strict=1")
	assert.Contains(t, out, "all or nothing: refused")
	assert.Equal(t, map[string]int{"ops:admit:a": 2, "ops:admit:b": 1}, p.TopicCounts(), "the refused subscription left nothing behind")
	out = tb.admit(t, "multi")
	assert.Contains(t, out, "both topics: held")
	assert.Equal(t, map[string]int{"ops:admit:a": 2, "ops:admit:b": 2}, p.TopicCounts(), "the full topic was skipped, the other joined")
	assert.Contains(t, tb.admit(t, "multi?strict=1"), "refused", "now both are full")

	assert.Contains(t, tb.admit(t, "add?policy=custom&topic="+adClosed), "refused")
	assert.Contains(t, tb.admit(t, "add?policy=custom&topic="+adSolo), "admitted")
	assert.Contains(t, tb.admit(t, "add?policy=custom&topic="+adSolo), "refused")
	assert.Contains(t, tb.admit(t, "add?policy=custom&topic="+adOpen), "admitted")

	for range adHeldMax {
		tb.admit(t, "add?policy=custom&topic="+adOpen)
	}
	assert.Contains(t, tb.admit(t, "add?policy=custom&topic="+adOpen), "not tried: the page holds at most 6")
	tb.app.Shutdown()
	for _, name := range l.order {
		assert.Zero(t, l.policies[name].broker.SubscriberCount(), "%s: shutdown releases the held subscriptions", name)
	}
}

func capture(t *testing.T, tb *testBoard, which string) string {
	t.Helper()
	return tb.admitlessPost(t, "/showcase/ops/wire/capture?which="+which)
}

func (tb *testBoard) admitlessPost(t *testing.T, path string) string {
	t.Helper()
	resp, err := tb.srv.Client().Post(tb.srv.URL+path, "", http.NoBody)
	require.NoError(t, err)
	defer resp.Body.Close()
	b := new(strings.Builder)
	_, _ = bufio.NewReader(resp.Body).WriteTo(b)
	return b.String()
}

func TestOpsWireLifetimesAndComments(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	sse := capture(t, tb, "sse")
	assert.Contains(t, sse, "event: tick", "the tick published during the capture")
	assert.Contains(t, sse, ": keepalive", "the broker's keepalive")
	assert.Contains(t, sse, "retry: 1500", "the reconnect delay, sent at the end of the lifetime")
	assert.Contains(t, sse, "the server ended the response", "the connection's lifetime is finite")

	custom := capture(t, tb, "custom")
	assert.Contains(t, custom, ": via custom writer\nevent: tick", "frames go through the custom writer")
	assert.Contains(t, custom, "retry: 2500", "the tick frame's own reconnect delay")
	assert.NotContains(t, custom, "retry: 1500", "StreamSSE has no lifetime here")
	assert.NotContains(t, custom, "the server ended the response")
	assert.GreaterOrEqual(t, strings.Count(custom, ": keepalive"), 6, "heartbeats every 700 ms and keepalives every second")
}

func TestOpsWireSyntheticFailures(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	for op, want := range map[string]string{"write": "WriteError at write", "flush": "WriteError at flush", "deadline": "WriteError at deadline"} {
		out := tb.admitlessPost(t, "/showcase/ops/wire/fail?op="+op)
		assert.Contains(t, out, "synthetic "+op+" failure: "+want, op)
		assert.Contains(t, out, "synthetic failure", op)
	}
}

// TestOpsWireRealWriteTimeout reads nothing from a real connection, so the
// server's writes eventually block and WithWriteTimeout ends the stream.
func TestOpsWireRealWriteTimeout(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.wire
	var got *tavern.WriteError
	var mu sync.Mutex
	h := l.broker.SSEHandler("ops:wire:timeout", tavern.WithWriteTimeout(200*time.Millisecond),
		tavern.WithWriteErrorHandler(func(e *tavern.WriteError) { mu.Lock(); got = e; mu.Unlock() }))
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return l.broker.HasSubscribers("ops:wire:timeout") }, time.Second, 5*time.Millisecond)
	big := "data: " + strings.Repeat("x", 256*1024) + "\n\n"
	require.Eventually(t, func() bool {
		l.broker.Publish("ops:wire:timeout", big)
		mu.Lock()
		defer mu.Unlock()
		return got != nil
	}, 15*time.Second, 10*time.Millisecond)
	assert.Equal(t, tavern.WriteOpWrite, got.Op)
	assert.True(t, errors.Is(got, os.ErrDeadlineExceeded), "cause: %v", got.Err)
}

// childStream opens a child stream. Its status is known once the response
// headers arrive, which a held stream does not send.
func childStream(t *testing.T, tb *testBoard, mode, conn string) (events <-chan recEvent, status func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/ops/child/events?mode="+mode+"&conn="+conn, http.NoBody)
	out := make(chan recEvent, 64)
	var code int
	ready := make(chan struct{})
	go func() {
		defer close(out)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			close(ready)
			return
		}
		code = resp.StatusCode
		close(ready)
		defer resp.Body.Close()
		readEvents(resp, out)
	}()
	return out, func() int {
		select {
		case <-ready:
		case <-time.After(2 * time.Second):
			t.Fatal("no response headers")
		}
		return code
	}
}

func TestOpsChildShutdownAndRestart(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.child
	c := l.current()
	live, _ := childStream(t, tb, "live", "l")
	childStream(t, tb, "held", "h")
	childStream(t, tb, "slow", "s")
	require.Eventually(t, func() bool { return c.reg.openStreams() == 3 }, time.Second, 5*time.Millisecond)
	waitFor(t, live, "child 1, tick", 2*time.Second)
	time.Sleep(2 * chTick) // the held stream's writes are waiting

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/child/shutdown", nil))
	retry := waitFor(t, live, "4000", time.Second)
	assert.Equal(t, "retry", retry.name, "the streams were told to wait 4 s before reconnecting")
	l.mu.Lock()
	report := l.report
	l.mu.Unlock()
	assert.Equal(t, 3, report["streamsBefore"])
	assert.Equal(t, 0, report["streamsLeft"], "live, held and slow streams all ended")
	assert.Equal(t, 0, report["subscriptionsLeft"])
	ticks := c.ticks.Load()
	time.Sleep(2 * chTick)
	assert.Equal(t, ticks, c.ticks.Load(), "the publisher stopped")
	_, status := childStream(t, tb, "live", "late")
	assert.Equal(t, http.StatusServiceUnavailable, status(), "a down child refuses streams")
	assert.Equal(t, http.StatusConflict, tb.post(t, "/showcase/ops/child/shutdown", nil))

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/ops/child/start", nil))
	assert.Equal(t, http.StatusConflict, tb.post(t, "/showcase/ops/child/start", nil))
	again, _ := childStream(t, tb, "live", "l")
	waitFor(t, again, "child 2, tick", 2*time.Second)
	assert.Equal(t, 0, c.reg.openStreams(), "nothing reached the old child")
	assert.Equal(t, http.StatusBadRequest, tb.status(t, "/showcase/ops/child/events?mode=fast&conn=z"))
}

func TestOpsShutdownReleasesEverything(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	o := tb.app.ops
	openTab(t, tb, "alice", "p")
	connsStream(t, tb, "bob", "c")
	childStream(t, tb, "held", "h")
	tb.admit(t, "add?policy=global&topic=ops:admit:a")
	require.Eventually(t, func() bool { return o.presence.reg.owner("p") != nil && o.conns.reg.owner("c") != nil }, time.Second, 5*time.Millisecond)
	c := o.child.current()
	tb.app.Shutdown()
	require.Eventually(t, func() bool {
		return o.presence.reg.openStreams() == 0 && o.conns.reg.openStreams() == 0 && c.reg.openStreams() == 0
	}, 2*time.Second, 10*time.Millisecond)
	for _, b := range o.brokers {
		assert.Zero(t, b.SubscriberCount())
	}
	assert.Zero(t, c.broker.SubscriberCount())
	assert.Nil(t, o.child.current(), "no child is left running")
}

// TestOpsChildShutdownEndsAStreamStillOpening shuts the child down while a
// held stream has subscribed but not yet held its writes. The stream must
// end with its generation, not whenever its client gives up.
func TestOpsChildShutdownEndsAStreamStillOpening(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.child
	c := l.current()
	reached, release := make(chan struct{}), make(chan struct{})
	l.opened = func(string) {
		close(reached)
		<-release
	}
	events, _ := childStream(t, tb, "held", "h")
	<-reached
	reports := make(chan map[string]any, 1)
	go func() { reports <- l.shutdown() }()
	require.Eventually(t, func() bool { return c.ctx.Err() != nil }, time.Second, time.Millisecond, "the generation is ending")
	close(release) // the stream now holds its writes, after the release pass

	select {
	case report := <-reports:
		assert.Equal(t, 1, report["streamsBefore"])
		assert.Equal(t, 0, report["streamsLeft"], "the opening stream ended too")
		assert.Equal(t, 0, report["subscriptionsLeft"])
	case <-time.After(chDrainMax + time.Second):
		t.Fatal("shutdown did not finish")
	}
	select {
	case _, ok := <-events:
		for ok {
			_, ok = <-events
		}
	case <-time.After(time.Second):
		t.Fatal("the request is still open")
	}
	assert.Zero(t, c.reg.openStreams(), "its reservation is released")
	assert.Zero(t, c.running())
}

func TestOpsAppShutdownSealsTheChild(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.child
	tb.app.Shutdown()
	assert.Nil(t, l.current())
	assert.Equal(t, http.StatusServiceUnavailable, tb.post(t, "/showcase/ops/child/start", nil), "no child starts after shutdown")
	assert.Nil(t, l.current())
	assert.ErrorIs(t, l.start(), errLabClosed)
	_, status := childStream(t, tb, "live", "late")
	assert.Equal(t, http.StatusServiceUnavailable, status())
}

// TestOpsChildTransitionsRaceTheFinalClose starts and stops children while
// the lab closes: every generation that was started is drained, and none
// is left running.
func TestOpsChildTransitionsRaceTheFinalClose(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.ops.child
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = l.start()
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				l.shutdown()
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	l.close()
	wg.Wait()
	l.close() // returns at once, after any shutdown still draining
	assert.Nil(t, l.current(), "nothing started after the close")
	l.mu.Lock()
	assert.Equal(t, l.gen, l.retired, "every generation was drained")
	l.mu.Unlock()
	assert.ErrorIs(t, l.start(), errLabClosed)
}

func TestLabRuntimeRefusesWorkAfterStop(t *testing.T) {
	r := newLabRuntime()
	ran := make(chan struct{})
	require.True(t, r.goBounded(func(ctx context.Context) { <-ctx.Done(); close(ran) }))
	r.stop()
	<-ran
	assert.False(t, r.goBounded(func(context.Context) { t.Error("work ran after stop") }))

	tb := newTestBoard(t, DefaultConfig())
	tb.app.Shutdown()
	assert.False(t, tb.app.delivery.timing.burst(1), "a burst is refused once the lab stops")
	assert.False(t, tb.app.delivery.timing.running.Load())
}
