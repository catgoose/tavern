package main

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (tb *testBoard) dlPost(t *testing.T, path string) int {
	t.Helper()
	return tb.post(t, "/showcase/delivery/"+path, nil)
}

// status is the status of a GET; a stream's body is closed unread.
func (tb *testBoard) status(t *testing.T, path string) int {
	t.Helper()
	resp, err := tb.srv.Client().Get(tb.srv.URL + path)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func dlStream(t *testing.T, tb *testBoard, family, conn string, query ...string) <-chan recEvent {
	t.Helper()
	path := "/showcase/delivery/" + family + "/events?conn=" + conn
	for _, q := range query {
		path += "&" + q
	}
	return sseStream(t, tb, path, "")
}

// containing returns the data of events that contain text.
func containing(events []recEvent, text string) []string {
	var out []string
	for _, ev := range events {
		if strings.Contains(ev.data, text) {
			out = append(out, ev.data)
		}
	}
	return out
}

// waitFor reads events until one contains text.
func waitFor(t *testing.T, events <-chan recEvent, text string, within time.Duration) recEvent {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "stream ended while waiting for %q", text)
			if strings.Contains(ev.data, text) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no event containing %q within %s", text, within)
		}
	}
}

func TestDeliveryChangeOnly(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.change
	events := dlStream(t, tb, "render", "a")
	waitFor(t, events, `id="render-counters"`, time.Second) // the snapshot
	blue, unblue := l.broker.SubscribeScoped(chTeam, "blue")
	defer unblue()

	for _, f := range []string{"status", "note", "team", "team-note"} {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/set?field="+f+"&value=amber"))
		waitFor(t, events, `id="`+changeFields[f].element+`"`, time.Second)
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/set?field="+f+"&value=amber"))
	}
	got := recUntilQuiet(events, 200*time.Millisecond)
	for _, f := range changeFields {
		assert.Empty(t, containing(got, `id="`+f.element+`"`), "%s: setting the same value again publishes nothing", f.label)
	}
	l.mu.Lock()
	assert.Equal(t, map[string]int{"status": 1, "note": 1, "team": 1, "team-note": 1}, maps.Clone(l.sent))
	assert.Equal(t, map[string]int{"status": 1, "note": 1, "team": 1, "team-note": 1}, maps.Clone(l.same))
	l.mu.Unlock()
	select {
	case m := <-blue:
		t.Fatalf("the blue team received a red team update: %q", m)
	default:
	}

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/clear-dedup?field=status"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/set?field=status&value=amber"))
	waitFor(t, events, `id="render-status"`, time.Second)
	assert.Equal(t, http.StatusBadRequest, tb.dlPost(t, "render/set?field=status&value=purple"))
}

func TestDeliveryLazyRendersOnlyForAnAudience(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.change
	counts := func() (calls, renders, published int) {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.lazy.calls, l.lazy.renders, l.lazy.published
	}
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report?team=1"))
	calls, renders, published := counts()
	assert.Equal(t, []int{2, 0, 0}, []int{calls, renders, published}, "no panel open: nothing is rendered")

	panel := sseStream(t, tb, "/showcase/delivery/render/report-events?conn=p", "")
	require.Eventually(t, func() bool { return l.broker.HasSubscribers(chReport) }, time.Second, 5*time.Millisecond)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report"))
	waitFor(t, panel, `id="render-report"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report?team=1"))
	waitFor(t, panel, `id="render-report"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report?changed=1"))
	waitFor(t, panel, `id="render-report"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/report?changed=1"))
	assert.Empty(t, recUntilQuiet(panel, 200*time.Millisecond), "an unchanged report is rendered but not published again")
	calls, renders, published = counts()
	assert.Equal(t, []int{6, 4, 3}, []int{calls, renders, published})
}

func TestDeliveryComponents(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.change
	events := dlStream(t, tb, "render", "a")
	waitFor(t, events, `id="render-counters"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/activity"))
	ev := waitFor(t, events, `id="render-activity"`, time.Second)
	assert.Contains(t, ev.data, `hx-swap-oob="afterbegin"`)
	assert.Contains(t, ev.data, `<li id="act-1">#1 status green</li>`)

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/activity?broken=1"))
	assert.True(t, l.log.has("activity #2 not published: this item cannot be rendered"))
	for range chActMax {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "render/activity"))
	}
	waitFor(t, events, `<div id="act-1" hx-swap-oob="delete">`, time.Second)
}

func TestDeliveryTimingConvergesAfterTheBurst(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.timing
	events := dlStream(t, tb, "timing", "a")
	waitFor(t, events, `id="timing-now"`, time.Second)
	require.Equal(t, http.StatusAccepted, tb.dlPost(t, "timing/burst?n=20"))
	assert.Equal(t, http.StatusConflict, tb.dlPost(t, "timing/burst?n=20"), "one burst at a time")
	require.Eventually(t, func() bool { return !l.running.Load() }, 3*time.Second, 10*time.Millisecond)
	got := recUntilQuiet(events, tmDebounce+tmThrottle+200*time.Millisecond)

	received := map[string]int{}
	final := map[string]string{}
	for _, ev := range got {
		for _, el := range []string{"timing-now", "timing-debounced", "timing-throttled"} {
			if strings.Contains(ev.data, `id="`+el+`"`) {
				received[el]++
				final[el] = ev.data
			}
		}
	}
	for el, data := range final {
		assert.Contains(t, data, `data-value="20"`, "%s ends on the final value", el)
	}
	assert.Equal(t, 20, received["timing-now"])
	assert.Equal(t, 1, received["timing-debounced"], "only after the quiet period")
	assert.GreaterOrEqual(t, received["timing-throttled"], 2, "the first at once and the latest later")
	assert.Less(t, received["timing-throttled"], 20)
	assert.Equal(t, int64(20), l.published[tmNow].Load())
	assert.Equal(t, int64(received["timing-debounced"]), l.published[tmDebounced].Load())
	assert.Equal(t, int64(received["timing-throttled"]), l.published[tmThrottled].Load())
}

func TestDeliveryTTL(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.ttl
	early := dlStream(t, tb, "ttl", "a")
	require.Eventually(t, func() bool { return l.broker.HasSubscribers(ttNotices) }, time.Second, 5*time.Millisecond)
	blue, unblue := l.broker.SubscribeScoped(ttTeam, "blue")
	defer unblue()

	require.Equal(t, http.StatusOK, tb.dlPost(t, "ttl/notice"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "ttl/banner"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "ttl/team"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "ttl/status?value=amber"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "ttl/status?value=amber"))
	live := recUntilQuiet(early, 200*time.Millisecond)
	assert.Len(t, containing(live, `id="notice-1"`), 1, "delivered at once")
	assert.Len(t, containing(live, `id="ttl-banner"`), 1)
	assert.Len(t, containing(live, `Team notice`), 1)
	assert.Len(t, containing(live, `id="ttl-status"`), 1, "the unchanged status is skipped")
	select {
	case m := <-blue:
		t.Fatalf("the blue team received a red team notice: %q", m)
	default:
	}

	later := recUntilQuiet(dlStream(t, tb, "ttl", "b"), 200*time.Millisecond)
	assert.Len(t, containing(later, `id="notice-1"`), 1, "a window opened before expiry gets the cached notice")
	assert.Len(t, containing(later, `id="ttl-banner"`), 1)

	removed := waitFor(t, early, `<div id="notice-1" hx-swap-oob="delete">`, ttTTL+time.Second)
	assert.NotEmpty(t, removed.data, "the notice is removed from an open window when it expires")
	rest := recUntilQuiet(early, 300*time.Millisecond)
	assert.Empty(t, containing(rest, "delete"), "the banner, status and team notice have no auto-removal")

	expired := recUntilQuiet(dlStream(t, tb, "ttl", "c"), 300*time.Millisecond)
	assert.Empty(t, expired, "a window opened after expiry gets nothing from the cache")
	assert.Equal(t, map[string]int{"published": 1, "skipped": 1}, l.statusSet)
}

func TestDeliveryTopicTTLDropsIdleTopics(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.ttl
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		resp, err := tb.srv.Client().Get(tb.srv.URL + "/showcase/delivery/ttl/events?conn=gone")
		if err == nil {
			resp.Body.Close()
		}
	}()
	require.Eventually(t, func() bool { return l.reg.owner("gone") != nil }, time.Second, 5*time.Millisecond)
	assert.Contains(t, strings.Join(l.windowTopics(), ","), "gone (1 subscribers)")
	<-closed
	require.Eventually(t, func() bool { return l.reg.owner("gone") == nil }, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return !strings.Contains(strings.Join(l.windowTopics(), ","), "gone") },
		3*ttIdle, 20*time.Millisecond, "the idle topic is dropped")
}

func TestDeliveryBatchIsOneMessagePerTopicAndScope(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.batch
	events := dlStream(t, tb, "batch", "a")
	require.Eventually(t, func() bool { return l.reg.owner("a") != nil && l.broker.HasSubscribers(btFeed) }, time.Second, 5*time.Millisecond)
	raw, unraw := l.broker.Subscribe(btBoard)
	defer unraw()

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "batch/flush"))
	got := recUntilQuiet(events, 200*time.Millisecond)
	require.Len(t, got, 4, "board, feed, team board and team feed")
	board := containing(got, "first board update")
	require.Len(t, board, 1)
	assert.Contains(t, board[0], "second board update", "both board updates in one message")
	select {
	case m := <-raw:
		assert.Contains(t, m, "first board update")
		assert.Contains(t, m, "second board update")
		assert.NotContains(t, m, "team", "a scoped update is not sent to unscoped subscribers")
	default:
		t.Fatal("the raw subscriber got nothing")
	}
	assert.Empty(t, raw, "exactly one channel message")
	l.mu.Lock()
	assert.Equal(t, map[string]int{"board": 1, "feed": 1, "board/red": 1, "feed/red": 1}, l.reads["a"])
	l.mu.Unlock()

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "batch/discard"))
	assert.Empty(t, recUntilQuiet(events, 200*time.Millisecond), "a discarded batch sends nothing")
	assert.Empty(t, raw)

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "batch/immediate"))
	got = recUntilQuiet(events, 200*time.Millisecond)
	require.Len(t, got, 3)
	assert.Len(t, containing(got, "PublishWithTTL, sent at once"), 1)
	var boardEvents []recEvent
	for _, ev := range got {
		if strings.Contains(ev.data, "batch-board") {
			boardEvents = append(boardEvents, ev)
		}
	}
	require.Len(t, boardEvents, 2, "per topic, the immediate message comes before the buffered one")
	assert.Contains(t, boardEvents[0].data, "PublishWithID, sent at once")
	assert.NotEmpty(t, boardEvents[0].id)
	assert.Contains(t, boardEvents[1].data, "buffered, sent at Flush")
}

func TestDeliveryOrderedTopicHasOneOrder(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.batch
	events := dlStream(t, tb, "batch", "a")
	require.Equal(t, http.StatusAccepted, tb.dlPost(t, "batch/order"))
	waitFor(t, events, `id="order-result"`, 5*time.Second)
	l.mu.Lock()
	result := l.order
	l.mu.Unlock()
	require.NotContains(t, result, "error")
	ordered := result[orOrdered].(map[string]any)
	assert.Equal(t, []int{orProducers * orEach, orProducers * orEach}, ordered["received"])
	assert.Equal(t, true, ordered["sameOrder"], "every subscriber of an ordered topic sees one order")
	assert.Equal(t, true, ordered["producerOrdered"])
	plain := result[orPlain].(map[string]any)
	assert.Equal(t, true, plain["producerOrdered"], "each producer's own messages stay in order either way")
}

func TestDeliveryShapingEndsOnTheLatestValue(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.shaping
	modes := []string{"plain", "rate", "coalesce", "rate-per-second", "team-rate", "team-coalesce", "meta", "tenths", "team-tenths"}
	streams := map[string]<-chan recEvent{}
	for _, m := range modes {
		streams[m] = dlStream(t, tb, "shaping", "a-"+m, "mode="+m)
	}
	require.Eventually(t, func() bool { return l.reg.openStreams() == len(modes) }, time.Second, 5*time.Millisecond)
	dropsBefore := l.broker.PublishDrops()
	require.Equal(t, http.StatusAccepted, tb.dlPost(t, "shaping/burst"))
	require.Eventually(t, func() bool { return !l.running.Load() }, 3*time.Second, 10*time.Millisecond)

	final := strconv.Itoa(shBurst)
	for _, m := range modes {
		got := recUntilQuiet(streams[m], 600*time.Millisecond)
		require.NotEmpty(t, got, m)
		last := got[len(got)-1].data
		switch m {
		case "plain", "meta":
			assert.NotEqual(t, final, last, "%s: an unshaped slow window drops values, including the final one", m)
			continue
		case "tenths", "team-tenths":
			assert.Equal(t, final, last, m)
			for _, ev := range got {
				assert.True(t, everyTenth(ev.data), "%s passes only every tenth value, got %s", m, ev.data)
			}
			continue
		}
		assert.Equal(t, final, last, "%s ends on the latest value", m)
		assert.Less(t, len(got), shBurst/2, "%s delivers far fewer than were published", m)
	}
	assert.Greater(t, l.broker.PublishDrops(), dropsBefore, "only the plain window's losses count as drops")
	assert.Equal(t, http.StatusBadRequest, tb.status(t, "/showcase/delivery/shaping/events?conn=x&mode=nope"))
}

func TestDeliveryPressureTiersAndRecovery(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.pressure
	l.repair.mu.Lock()
	l.repair.base, l.repair.max, l.repair.delay = 50*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	l.repair.mu.Unlock()
	current := func() string {
		l.mu.Lock()
		defer l.mu.Unlock()
		return `data-value="` + strconv.Itoa(l.value) + `"`
	}

	events := dlStream(t, tb, "pressure", "a")
	waitFor(t, events, `id="gauge"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/change?n=10"))
	require.True(t, l.log.has("a: normal → throttle"), "consecutive drops raise the tier")
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=0"))
	waitFor(t, events, current(), 2*time.Second)
	require.Eventually(t, func() bool { return l.log.has("→ normal") }, 2*time.Second, 10*time.Millisecond, "a successful send resets the tier")
	if l.log.has("→ simplify") {
		t.Log("reached the simplify tier")
	}

	// Blocking publishes: a full queue times out, a free one does not. The
	// repair is slowed so the held window is not evicted meanwhile.
	recUntilQuiet(events, 300*time.Millisecond)
	l.repair.mu.Lock()
	l.repair.base, l.repair.max, l.repair.delay = time.Second, time.Second, time.Second
	l.repair.mu.Unlock()
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/change?n=10"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/blocking"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/blocking?team=1"))
	l.mu.Lock()
	results := append([]string{}, l.results...)
	l.mu.Unlock()
	require.Len(t, results, 2)
	assert.Contains(t, results[1], "to everyone: tavern: publish timeout after 3")
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=0"))
	waitFor(t, events, current(), 3*time.Second)
	recUntilQuiet(events, 300*time.Millisecond)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/blocking?team=1"))
	l.mu.Lock()
	assert.Contains(t, l.results[0], "to the red team: delivered in")
	l.mu.Unlock()

	// Held long enough, the window is evicted and its reconnect starts from
	// the current value.
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/change?n=30"))
	require.Eventually(t, func() bool { return l.log.has("→ disconnect") }, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=a&on=0"))
	require.Eventually(t, func() bool { return l.reg.owner("a") == nil }, 3*time.Second, 10*time.Millisecond, "the evicted stream ends")
	again := dlStream(t, tb, "pressure", "a")
	waitFor(t, again, current(), time.Second)
}

func TestDeliverySimplifiedContentKeepsTheValue(t *testing.T) {
	full := tavern.RenderFragments(tavern.Replace("gauge", gaugeHTML(42)))
	simple := simplifyGauge(full)
	assert.Contains(t, simple, `data-value="42"`)
	assert.Contains(t, simple, `data-fidelity="simplified"`)
	assert.NotContains(t, simple, "<table>")
	assert.Less(t, len(simple), len(full))
}

func TestDeliveryReactiveWorkflow(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.flow
	events := dlStream(t, tb, "flow", "a")
	waitFor(t, events, `id="audit-rows"`, time.Second) // the snapshot
	require.Eventually(t, func() bool { return l.log.has("worker 1 started") }, time.Second, 5*time.Millisecond, "the first subscriber starts the worker")

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=tea"))
	// The three topics are separate subscriptions with no order between
	// them, so collect them all.
	first := recUntilQuiet(events, 300*time.Millisecond)
	row := containing(first, `id="order-1"`)
	require.Len(t, row, 1)
	assert.Contains(t, row[0], checked, "order topics pass through the orders:* middleware")
	audit := containing(first, "order #1 added")
	require.Len(t, audit, 1)
	assert.Contains(t, audit[0], unchecked, "the audit topic does not match orders:*")
	summary := containing(first, `id="order-summary"`)
	require.Len(t, summary, 1, "one derived summary after the list publish")
	assert.Contains(t, summary[0], `data-orders="1"`)

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=tea&test=1"))
	got := recUntilQuiet(events, 300*time.Millisecond)
	assert.Empty(t, containing(got, `id="order-2"`), "the middleware holds back the test row")
	assert.Len(t, containing(got, "order #2 added (test)"), 1)
	assert.Empty(t, containing(got, `id="order-summary"`), "no list publish, so no derived summary")

	for range flKept {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=cake"))
	}
	waitFor(t, events, `<div id="order-1" hx-swap-oob="delete">`, time.Second)
	l.mu.Lock()
	steps := maps.Clone(l.counts)
	seen := maps.Clone(l.seen)
	l.mu.Unlock()
	assert.Equal(t, 2+flKept, steps["mutations"])
	assert.Equal(t, 1, steps["filtered by orders:* middleware"])
	assert.Positive(t, seen[flAudit], "the global middleware sees every topic")
	assert.Positive(t, seen[flList])
	assert.Equal(t, http.StatusBadRequest, tb.dlPost(t, "flow/order?item=soup"))
}

func TestDeliveryWorkerStopsWithTheLastSubscriber(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.flow
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := tb.srv.Client().Get(tb.srv.URL + "/showcase/delivery/flow/events?conn=w")
		if err == nil {
			resp.Body.Close()
		}
	}()
	require.Eventually(t, func() bool { return l.log.has("worker 1 started") }, time.Second, 5*time.Millisecond)
	<-done
	require.Eventually(t, func() bool { return l.log.has("worker 1 stopped") }, time.Second, 5*time.Millisecond)
	events := dlStream(t, tb, "flow", "x")
	require.Eventually(t, func() bool { return l.log.has("worker 2 started") }, time.Second, 5*time.Millisecond, "a later first subscriber starts it again")
	waitFor(t, events, "worker 2 at", flTick+time.Second)
}

func TestDeliveryScheduledSections(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.sched
	attempts := func(section string) int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.attempts[section]
	}
	time.Sleep(3 * scLoad)
	assert.Zero(t, attempts("clock")+attempts("load"), "nothing is rendered without a subscriber")

	events := dlStream(t, tb, "sched", "a")
	waitFor(t, events, `data-state="live"`, 2*time.Second)
	waitFor(t, events, `id="dash-clock"`, 2*time.Second)

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "sched/fail?on=1"))
	waitFor(t, events, `data-state="fallback"`, 2*time.Second)
	assert.True(t, l.log.has("section load failed (2 in a row): load source unavailable"))
	l.mu.Lock()
	errs := l.errors
	l.mu.Unlock()
	time.Sleep(scRecover / 2)
	l.mu.Lock()
	assert.Equal(t, errs, l.errors, "while the circuit is open the source is not tried")
	l.mu.Unlock()

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "sched/fail?on=0"))
	waitFor(t, events, `data-state="live"`, scRecover+time.Second)

	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "sched/interval?fast=1"))
	before := attempts("clock")
	time.Sleep(time.Second)
	assert.GreaterOrEqual(t, attempts("clock")-before, 2, "the faster interval takes effect")

	tb.app.Shutdown()
	after := attempts("clock") + attempts("load")
	time.Sleep(3 * scClockAlt)
	assert.Equal(t, after, attempts("clock")+attempts("load"), "shutdown stops the publisher")
}

func TestDeliveryConnectionsHaveOneOwner(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.pressure
	events := dlStream(t, tb, "pressure", "same")
	waitFor(t, events, `id="gauge"`, time.Second)
	assert.Equal(t, http.StatusConflict, tb.status(t, "/showcase/delivery/pressure/events?conn=same"))
	assert.Equal(t, http.StatusNotFound, tb.dlPost(t, "pressure/pause?conn=other&on=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=same&on=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/change?n=1"))
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=same&on=0"))
	waitFor(t, events, `data-value="1"`, time.Second)
	assert.Equal(t, 1, l.reg.openStreams())
}

func TestDeliveryShutdownReleasesEverything(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	d := tb.app.delivery
	families := map[string]*connRegistry{
		"render": &d.change.reg, "timing": &d.timing.reg, "ttl": &d.ttl.reg, "batch": &d.batch.reg,
		"pressure": &d.pressure.reg, "flow": &d.flow.reg, "sched": &d.sched.reg,
	}
	for f, reg := range families {
		dlStream(t, tb, f, "s")
		require.Eventually(t, func() bool { return reg.owner("s") != nil }, time.Second, 5*time.Millisecond, f)
	}
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "pressure/pause?conn=s&on=1"))
	require.Equal(t, http.StatusAccepted, tb.dlPost(t, "timing/burst?n=40"))
	tb.app.Shutdown()
	for f, reg := range families {
		require.Eventually(t, func() bool { return reg.openStreams() == 0 }, 2*time.Second, 10*time.Millisecond, f)
	}
	for _, b := range d.brokers {
		assert.Zero(t, b.SubscriberCount())
	}
}

// TestDeliveryNoticesStayWithinRetention posts more notices than the replay
// cache keeps. A notice pushed out of the cache would never be removed by
// expiry, so the sixth is refused while five show; all five then leave the
// open window when they expire, and posting works again.
func TestDeliveryNoticesStayWithinRetention(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.ttl
	events := dlStream(t, tb, "ttl", "a")
	require.Eventually(t, func() bool { return l.broker.HasSubscribers(ttNotices) }, time.Second, 5*time.Millisecond)
	for range ttKept {
		assert.True(t, l.notice())
	}
	assert.False(t, l.notice(), "a sixth notice is refused while five show")
	assert.True(t, l.log.has("notice refused: 5 are showing"))
	for range 3 {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "ttl/team"))
	}
	later := recUntilQuiet(dlStream(t, tb, "ttl", "b"), 300*time.Millisecond)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, liveIDs(later, "notice"), "a later window gets the five showing")
	team := containing(later, "Team notice")
	require.Len(t, team, 1, "the team notice is one slot")
	assert.Contains(t, team[0], "Team notice 8")

	got := recUntilQuiet(events, ttTTL+time.Second)
	assert.Empty(t, liveIDs(got, "notice"), "every notice has left the open window")
	assert.True(t, l.notice(), "once they expire, notices can be posted again")
	l.mu.Lock()
	assert.Len(t, l.showing, 1)
	l.mu.Unlock()
}

var (
	insertedID = regexp.MustCompile(`<li id="([a-z]+)-(\d+)"`)
	deletedID  = regexp.MustCompile(`<div id="([a-z]+)-(\d+)" hx-swap-oob="delete">`)
)

// liveIDs replays inserts and deletes of prefix-N elements and returns the
// IDs left, in order.
func liveIDs(events []recEvent, prefix string) []int {
	live := map[int]bool{}
	for _, ev := range events {
		for _, m := range insertedID.FindAllStringSubmatch(ev.data, -1) {
			if m[1] == prefix {
				n, _ := strconv.Atoi(m[2])
				live[n] = true
			}
		}
		for _, m := range deletedID.FindAllStringSubmatch(ev.data, -1) {
			if m[1] == prefix {
				n, _ := strconv.Atoi(m[2])
				delete(live, n)
			}
		}
	}
	return slices.Sorted(maps.Keys(live))
}

func modelIDs(l *flowLab) (visible []int, summary string) {
	l.mu.Lock()
	for _, o := range l.orders {
		if !o.Test {
			visible = append(visible, o.ID)
		}
	}
	l.mu.Unlock()
	slices.Sort(visible)
	return visible, l.summaryHTML()
}

// TestDeliveryWorkflowWindowsStartFromTheModel opens windows after orders
// were placed: each starts from the model's rows, summary and audit log.
func TestDeliveryWorkflowWindowsStartFromTheModel(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.flow
	for range 3 {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=tea"))
	}
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=cake&test=1"))
	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.counts["derived summaries"] == 3
	}, time.Second, 5*time.Millisecond)
	for _, conn := range []string{"late", "reconnected"} {
		snap := recNext(t, dlStream(t, tb, "flow", conn))
		assert.Equal(t, []int{1, 2, 3}, liveIDs([]recEvent{snap}, "order"), "%s: the visible rows, without the test order", conn)
		assert.Contains(t, snap.data, checked)
		assert.Contains(t, snap.data, "3 orders shown: tea 3")
		assert.Equal(t, []int{1, 2, 3, 4}, liveIDs([]recEvent{snap}, "audit"), "%s: the audit log", conn)
		assert.Contains(t, snap.data, "order #4 added (test) "+unchecked, "the audit log is not an order topic")
	}
}

// TestDeliveryWorkflowMutationsStayInOrder holds the first order's row in
// middleware while more orders are placed. A later mutation must not
// publish first: the first row would then be inserted after its removal.
func TestDeliveryWorkflowMutationsStayInOrder(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.flow
	events := dlStream(t, tb, "flow", "a")
	waitFor(t, events, `id="audit-rows"`, time.Second)

	reached, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	l.broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			if strings.Contains(msg, `<li id="order-1"`) && held.CompareAndSwap(false, true) {
				close(reached)
				<-release
			}
			next(topic, msg)
		}
	})
	first, rest := make(chan struct{}), make(chan struct{})
	go func() { defer close(first); l.add("tea", false) }()
	<-reached
	go func() {
		defer close(rest)
		for range flKept {
			l.add("cake", false)
		}
	}()
	select {
	case <-rest:
	case <-time.After(200 * time.Millisecond): // the later orders wait for the first
	}
	close(release)
	<-first
	<-rest
	require.Eventually(t, func() bool { // one summary per list publish, removals included
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.counts["derived summaries"] == l.seen[flList]
	}, 2*time.Second, 5*time.Millisecond)

	got := recUntilQuiet(events, 300*time.Millisecond)
	visible, summary := modelIDs(l)
	assert.Equal(t, []int{2, 3, 4, 5, 6, 7}, visible)
	assert.Equal(t, visible, liveIDs(got, "order"), "the window's rows match the model")
	summaries := containing(got, `id="order-summary"`)
	require.NotEmpty(t, summaries)
	want, _ := checkOrders(summary)
	assert.Contains(t, summaries[len(summaries)-1], want, "the last summary is the model's")
}

// TestDeliveryWorkflowRemovesOnlyShownRows ages a held-back test order out
// of the model: no removal is sent for a row no window has.
func TestDeliveryWorkflowRemovesOnlyShownRows(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	events := dlStream(t, tb, "flow", "a")
	waitFor(t, events, `id="audit-rows"`, time.Second)
	require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=tea&test=1"))
	for range flKept + 1 {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=cake"))
	}
	got := recUntilQuiet(events, 300*time.Millisecond)
	assert.Empty(t, containing(got, `<div id="order-1" hx-swap-oob="delete">`), "the test order was never shown")
	assert.Len(t, containing(got, `<div id="order-2" hx-swap-oob="delete">`), 1)
	assert.Equal(t, []int{3, 4, 5, 6, 7, 8}, liveIDs(got, "order"))
}

func TestDeliveryWorkflowAuditIsBounded(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.delivery.flow
	events := dlStream(t, tb, "flow", "a")
	waitFor(t, events, `id="audit-rows"`, time.Second)
	for range flAuditMax + 2 {
		require.Equal(t, http.StatusNoContent, tb.dlPost(t, "flow/order?item=tea"))
	}
	got := recUntilQuiet(events, 300*time.Millisecond)
	assert.Equal(t, []int{3, 4, 5, 6, 7, 8, 9, 10}, liveIDs(got, "audit"))
	assert.Equal(t, []int{5, 6, 7, 8, 9, 10}, liveIDs(got, "order"))
	l.mu.Lock()
	assert.Len(t, l.audit, flAuditMax)
	assert.Len(t, l.orders, flKept)
	l.mu.Unlock()
}
