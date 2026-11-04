package main

import (
	"context"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
)

// The batch family buffers several publishes and flushes them as one
// channel message per topic and scope; the ordering part runs concurrent
// producers on an ordered and a plain topic and compares what two
// subscribers of each saw.
const (
	btBoard     = "delivery/batch/board"
	btFeed      = "delivery/batch/feed"
	btResult    = "delivery/batch/result"
	orOrdered   = "delivery/order/ordered"
	orPlain     = "delivery/order/plain"
	orProducers = 4
	orEach      = 25
	btKept      = 12 // list items kept on the page per list
)

type batchLab struct {
	d       *delivery
	broker  *tavern.SSEBroker
	ordered *tavern.SSEBroker // large queues, so the observers drop nothing
	reg     connRegistry

	mu        sync.Mutex
	seq       int
	flushed   int
	discarded int
	reads     map[string]map[string]int // conn → subscription → channel messages
	order     map[string]any            // the last ordering run
	running   atomic.Bool
}

func newBatchLab(d *delivery) *batchLab {
	l := &batchLab{
		d:       d,
		broker:  d.broker(tavern.WithBufferSize(16)),
		ordered: d.broker(tavern.WithBufferSize(4 * orProducers * orEach)),
		reads:   map[string]map[string]int{},
	}
	l.ordered.SetOrdered(orOrdered, true)
	return l
}

func (l *batchLab) next() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return l.seq
}

func item(list, text string) tavern.Fragment {
	return tavern.Append(list, "<li>"+template.HTMLEscapeString(text)+"</li>")
}

// build buffers one batch: two board updates and a feed item for everyone,
// and one of each for the red team.
func (l *batchLab) build() *tavern.PublishBatch {
	n := l.next()
	b := l.broker.Batch()
	b.Publish(btBoard, tavern.RenderFragments(item("batch-board", fmt.Sprintf("batch %d: first board update", n))))
	b.Publish(btBoard, tavern.RenderFragments(item("batch-board", fmt.Sprintf("batch %d: second board update", n))))
	b.PublishOOB(btFeed, item("batch-feed", fmt.Sprintf("batch %d: feed item", n)))
	b.PublishTo(btBoard, deliveryTeam, tavern.RenderFragments(item("batch-team", fmt.Sprintf("batch %d: team board update", n))))
	b.PublishOOBTo(btFeed, deliveryTeam, item("batch-team", fmt.Sprintf("batch %d: team feed item", n)))
	return b
}

func (l *batchLab) flush() {
	b := l.build()
	b.Flush()
	l.mu.Lock()
	l.flushed++
	l.mu.Unlock()
}

func (l *batchLab) discard() {
	b := l.build()
	b.Discard()
	l.mu.Lock()
	l.discarded++
	l.mu.Unlock()
}

// immediate shows that PublishWithID and PublishWithTTL on a batch are not
// buffered: they are delivered before the batch's buffered update, which
// waits for Flush.
func (l *batchLab) immediate() {
	n := l.next()
	b := l.broker.Batch()
	b.Publish(btBoard, tavern.RenderFragments(item("batch-board", fmt.Sprintf("batch %d: buffered, sent at Flush", n))))
	b.PublishWithID(btBoard, strconv.Itoa(n),
		tavern.NewSSEMessage("oob", tavern.RenderFragments(item("batch-board", fmt.Sprintf("batch %d: PublishWithID, sent at once", n)))).String())
	b.PublishWithTTL(btFeed, tavern.RenderFragments(item("batch-feed", fmt.Sprintf("batch %d: PublishWithTTL, sent at once", n))), ttTTL)
	b.Flush()
}

func (l *batchLab) onRead(conn string, m tavern.TopicMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reads[conn] == nil {
		l.reads[conn] = map[string]int{}
	}
	l.reads[conn][m.Topic]++
}

func (l *batchLab) ended(conn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reads, conn)
}

func (l *batchLab) open(conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	meta := tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn})
	team := tavern.SubWithScope(deliveryTeam)
	sub := func(b *tavern.SSEBroker, key, topic string, opts ...tavern.SubscribeOption) func() (keyedSub, error) {
		return func() (keyedSub, error) {
			ch, unsub := b.SubscribeWith(topic, append(opts, meta)...)
			return subOrErr(key, ch, unsub)
		}
	}
	return openAll(
		sub(l.broker, "board", btBoard), sub(l.broker, "feed", btFeed),
		sub(l.broker, "board/red", btBoard, team), sub(l.broker, "feed/red", btFeed, team),
		sub(l.ordered, "result", btResult),
	)
}

// runOrder starts concurrent producers on both topics, with two observers
// on each, and publishes what the observers saw. One run at a time.
func (l *batchLab) runOrder() bool {
	if !l.running.CompareAndSwap(false, true) {
		return false
	}
	l.d.goBounded(func(ctx context.Context) {
		defer l.running.Store(false)
		result := l.order1(ctx)
		l.mu.Lock()
		l.order = result
		l.mu.Unlock()
		l.ordered.PublishOOB(btResult, tavern.Replace("order-result", orderHTML(result)))
	})
	return true
}

func (l *batchLab) order1(ctx context.Context) map[string]any {
	type observer struct {
		ch    <-chan string
		unsub func()
		seen  []string
	}
	topics := []string{orOrdered, orPlain}
	obs := map[string][]*observer{}
	for _, t := range topics {
		for range 2 {
			ch, unsub := l.ordered.Subscribe(t)
			obs[t] = append(obs[t], &observer{ch: ch, unsub: unsub})
		}
	}
	defer func() {
		for _, list := range obs {
			for _, o := range list {
				o.unsub()
			}
		}
	}()
	var wg sync.WaitGroup
	for p := range orProducers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range orEach {
				msg := fmt.Sprintf("p%d-%02d", p, i)
				for _, t := range topics {
					l.ordered.Publish(t, msg)
				}
			}
		}()
	}
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for _, list := range obs {
		for _, o := range list {
			for len(o.seen) < orProducers*orEach {
				select {
				case m := <-o.ch:
					o.seen = append(o.seen, m)
				case <-deadline:
					return map[string]any{"error": "observers did not receive every message"}
				case <-ctx.Done():
					return map[string]any{"error": "stopped"}
				}
			}
		}
	}
	out := map[string]any{}
	for _, t := range topics {
		a, b := obs[t][0].seen, obs[t][1].seen
		out[t] = map[string]any{
			"received":        []int{len(a), len(b)},
			"sameOrder":       slices.Equal(a, b),
			"producerOrdered": producerOrdered(a) && producerOrdered(b),
			"first":           a[:8],
		}
	}
	return out
}

// producerOrdered reports whether each producer's messages appear in the
// order it sent them.
func producerOrdered(seen []string) bool {
	last := map[string]string{}
	for _, m := range seen {
		p, i, _ := strings.Cut(m, "-")
		if prev, ok := last[p]; ok && i <= prev {
			return false
		}
		last[p] = i
	}
	return true
}

func orderHTML(result map[string]any) string {
	if e, ok := result["error"]; ok {
		return template.HTMLEscapeString(fmt.Sprint(e))
	}
	var b strings.Builder
	for _, t := range []string{orOrdered, orPlain} {
		r := result[t].(map[string]any)
		fmt.Fprintf(&b, `<p data-topic="%s" data-same="%t">%s: both subscribers received %v; same order: <strong>%t</strong>; each producer in order: %t; first: %s</p>`,
			t, r["sameOrder"], t, r["received"], r["sameOrder"], r["producerOrdered"], template.HTMLEscapeString(strings.Join(r["first"].([]string), " ")))
	}
	return b.String()
}

func (a *App) registerBatch() {
	l := a.delivery.batch
	a.mux.HandleFunc("GET /showcase/delivery/batch", func(w http.ResponseWriter, _ *http.Request) {
		a.renderDeliveryPage(w, batchPage, map[string]any{"Title": "Batches and ordering", "Family": "batch"})
	})
	a.mux.HandleFunc("GET /showcase/delivery/batch/events", func(w http.ResponseWriter, r *http.Request) {
		a.serveOwned(w, r, ownedStream{reg: &l.reg, open: l.open, onRead: l.onRead, ended: l.ended})
	})
	for path, fn := range map[string]func(){"flush": l.flush, "discard": l.discard, "immediate": l.immediate} {
		a.mux.HandleFunc("POST /showcase/delivery/batch/"+path, func(w http.ResponseWriter, _ *http.Request) {
			fn()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	a.mux.HandleFunc("POST /showcase/delivery/batch/order", func(w http.ResponseWriter, _ *http.Request) {
		if !l.runOrder() {
			http.Error(w, "A run is already in progress", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	a.mux.HandleFunc("GET /showcase/delivery/batch/state", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		writeJSON(w, map[string]any{
			"flushed": l.flushed, "discarded": l.discarded,
			"channelMessages": maps.Clone(l.reads[r.URL.Query().Get("conn")]), "order": l.order,
		})
	})
}

var batchPage = deliveryPage("batch", `{{define "main"}}
<p class="note">A batch holds two board updates and a feed item for everyone, and a board update and feed item for the red team, which this page joins. Flushing sends each topic and scope one channel message, so this window's four subscriptions get one message each. A discarded batch sends nothing. PublishWithID and PublishWithTTL on a batch are not buffered: they go at once, before the buffered update.</p>
<section id="batch-region" sse-connect="/showcase/delivery/batch/events?conn={{.Conn}}">
  <div sse-swap="oob" hx-swap="none"></div>
  <h2>Board</h2><ul id="batch-board"></ul>
  <h2>Feed</h2><ul id="batch-feed"></ul>
  <h2>Team</h2><ul id="batch-team"></ul>
  <h2>Ordering</h2>
  <div id="order-result"><p>Not run yet.</p></div>
</section>
<p>
  <button hx-post="/showcase/delivery/batch/flush" hx-swap="none">Publish a batch</button>
  <button hx-post="/showcase/delivery/batch/discard" hx-swap="none">Build a batch and discard it</button>
  <button hx-post="/showcase/delivery/batch/immediate" hx-swap="none">Batch with an immediate message</button>
  <button hx-post="/showcase/delivery/batch/order" hx-swap="none">Run concurrent producers</button>
</p>
<p class="note">Four producers each publish 25 numbered messages to an ordered topic (SetOrdered) and a plain one, at the same time. Two subscribers of the ordered topic always see the same order. On the plain topic they may differ. Either way each producer's own messages stay in its order, and there is no order across topics.</p>
{{end}}`)
