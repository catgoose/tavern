package main

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

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
