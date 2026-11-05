package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientStreamOpensWithTheCurrentValueAndReleasesOnClose(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.client
	l.pulse()

	resp, err := tb.srv.Client().Get(tb.srv.URL + "/showcase/client?debug=1")
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(page), "tavern-debug")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/client/events?conn=c1", http.NoBody)
	out := make(chan recEvent, 16)
	go func() {
		defer close(out)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		readEvents(resp, out)
	}()

	first := <-out
	assert.Equal(t, recEvent{name: "pulse", data: "pulse 1"}, first, "the current value comes first")
	require.Eventually(t, func() bool { return l.broker.SubscriberCount() == 1 }, time.Second, 5*time.Millisecond)
	l.pulse()
	assert.Equal(t, "pulse 2", (<-out).data)

	cancel()
	require.Eventually(t, func() bool { return l.broker.SubscriberCount() == 0 }, time.Second, 5*time.Millisecond, "closing the connection ends the subscription")
	assert.Zero(t, l.reg.openStreams())
}

func clientStream(t *testing.T, tb *testBoard, conn string) <-chan recEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/client/events?conn="+conn, http.NoBody)
	out := make(chan recEvent, 16)
	go func() {
		defer close(out)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		readEvents(resp, out)
	}()
	return out
}

func nextPulse(t *testing.T, events <-chan recEvent) string {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "stream ended")
		return ev.data
	case <-time.After(2 * time.Second):
		t.Fatal("no pulse within 2s")
		return ""
	}
}

// TestClientPulsesArriveInOrder holds the first pulse's publish with a
// middleware barrier while a second pulse starts. The second waits for the
// first, so every stream gets them in order.
func TestClientPulsesArriveInOrder(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.client
	streams := []<-chan recEvent{clientStream(t, tb, "a"), clientStream(t, tb, "b")}
	for _, s := range streams {
		assert.Equal(t, "pulse 0", nextPulse(t, s))
	}
	require.Eventually(t, func() bool { return l.broker.SubscriberCount() == 2 }, time.Second, 5*time.Millisecond)

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	l.broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
		return func(topic, msg string) {
			if strings.Contains(msg, "data: pulse 1\n") {
				close(reached)
				<-release
			}
			next(topic, msg)
		}
	})
	first := make(chan struct{})
	go func() { l.pulse(); close(first) }()
	<-reached
	second := make(chan struct{})
	go func() { l.pulse(); close(second) }()
	select {
	case <-second:
		t.Error("the second pulse published while the first was still publishing")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	<-first
	<-second

	for _, s := range streams {
		assert.Equal(t, "pulse 1", nextPulse(t, s))
		assert.Equal(t, "pulse 2", nextPulse(t, s))
	}
	assert.Equal(t, int64(2), l.pulses())
}

// TestClientStreamSetupMissesNoPulse holds a new stream between taking its
// snapshot and subscribing, and pulses meanwhile. The pulse waits for the
// subscription, so the stream ends on the last pulse.
func TestClientStreamSetupMissesNoPulse(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	l := tb.app.client
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	l.opening = func() {
		l.opening = nil
		close(reached)
		<-release
	}
	events := clientStream(t, tb, "late")
	<-reached
	pulsed := make(chan struct{})
	go func() { l.pulse(); close(pulsed) }()
	select {
	case <-pulsed:
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	<-pulsed

	assert.Equal(t, "pulse 0", nextPulse(t, events), "the snapshot comes first")
	assert.Equal(t, "pulse 1", nextPulse(t, events), "then the pulse made during setup")
	assert.Equal(t, int64(1), l.pulses())
}
