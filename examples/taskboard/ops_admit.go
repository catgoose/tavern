package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The admission page holds subscriptions on three brokers, each with one
// kind of limit, and adds or removes them on request. The page's own
// stream is on none of them, so it never uses up a limit.
const (
	adGlobalMax = 3 // WithMaxSubscribers
	adTopicMax  = 2 // WithMaxSubscribersPerTopic
	adHeldMax   = 6 // subscriptions the page holds per broker
	adSolo      = "ops:admit:solo"
	adClosed    = "ops:admit:closed"
	adOpen      = "ops:admit:open"
)

var adTopics = map[string][]string{
	"global": {"ops:admit:a", "ops:admit:b"},
	"topic":  {"ops:admit:a", "ops:admit:b"},
	"custom": {adOpen, adSolo, adClosed},
}

type admitPolicy struct {
	name, rule string
	broker     *tavern.SSEBroker
}

type heldSub struct {
	topic string
	unsub func()
}

type admitLab struct {
	policies map[string]*admitPolicy
	order    []string

	mu   sync.Mutex
	held map[string][]heldSub // policy → held subscriptions, oldest first
	last map[string]string    // policy → the last result
	log  obsLog
}

func newAdmitLab(o *ops) *admitLab {
	l := &admitLab{held: map[string][]heldSub{}, last: map[string]string{}, order: []string{"global", "topic", "custom"}}
	l.policies = map[string]*admitPolicy{
		"global": {name: "global", rule: fmt.Sprintf("at most %d subscriptions on the broker", adGlobalMax),
			broker: o.broker(tavern.WithMaxSubscribers(adGlobalMax))},
		"topic": {name: "topic", rule: fmt.Sprintf("at most %d subscriptions per topic", adTopicMax),
			broker: o.broker(tavern.WithMaxSubscribersPerTopic(adTopicMax))},
		"custom": {name: "custom", rule: adOpen + " is open, " + adSolo + " takes one subscription, " + adClosed + " takes none",
			broker: o.broker(tavern.WithAdmissionControl(func(topic string, current int) bool {
				// Runs synchronously on every subscription attempt: decide only.
				switch topic {
				case adOpen:
					return true
				case adSolo:
					return current < 1
				}
				return false
			}))},
	}
	return l
}

// add tries one subscription on the policy's broker and holds it if
// admitted.
func (l *admitLab) add(policy, topic string) string {
	p := l.policies[policy]
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.held[policy]) >= adHeldMax {
		return l.result(policy, "not tried: the page holds at most %d here", adHeldMax)
	}
	ch, unsub := p.broker.Subscribe(topic)
	if ch == nil {
		return l.result(policy, "%s refused (%s); %d held", topic, p.rule, len(l.held[policy]))
	}
	l.held[policy] = append(l.held[policy], heldSub{topic, unsub})
	return l.result(policy, "%s admitted; %d held", topic, len(l.held[policy]))
}

// remove ends the oldest held subscription, making room.
func (l *admitLab) remove(policy string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.held[policy]
	if len(h) == 0 {
		return l.result(policy, "nothing to remove")
	}
	h[0].unsub()
	l.held[policy] = h[1:]
	return l.result(policy, "%s removed; %d held", h[0].topic, len(l.held[policy]))
}

// multi subscribes to both topics at once on the per-topic broker.
// SubscribeMulti skips a topic that is full and holds the rest; with
// allOrNothing (SubWithTerminateOnInnerClose) a full topic refuses the
// whole subscription and undoes the topics already admitted.
func (l *admitLab) multi(allOrNothing bool) string {
	p := l.policies["topic"]
	topics := adTopics["topic"]
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.held["topic"]) >= adHeldMax {
		return l.result("topic", "not tried: the page holds at most %d here", adHeldMax)
	}
	before := p.broker.TopicCounts()
	var ch <-chan tavern.TopicMessage
	var unsub func()
	if allOrNothing {
		ch, unsub = p.broker.SubscribeMultiWith(topics, tavern.SubWithTerminateOnInnerClose())
	} else {
		ch, unsub = p.broker.SubscribeMulti(topics...)
	}
	after := p.broker.TopicCounts()
	if ch == nil {
		return l.result("topic", "both topics, all or nothing: refused; counts before %v, after %v", before, after)
	}
	l.held["topic"] = append(l.held["topic"], heldSub{"both", unsub})
	kind := "both topics"
	if allOrNothing {
		kind = "both topics, all or nothing"
	}
	return l.result("topic", "%s: held; counts before %v, after %v", kind, before, after)
}

func (l *admitLab) result(policy, format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	l.last[policy] = s
	l.log.add("%s: %s", policy, s)
	return s
}

func (l *admitLab) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for p, h := range l.held {
		for _, s := range h {
			s.unsub()
		}
		delete(l.held, p)
	}
}

func (l *admitLab) state() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]any{"observations": l.log.list()}
	for _, name := range l.order {
		p := l.policies[name]
		var topics []string
		for _, h := range l.held[name] {
			topics = append(topics, h.topic)
		}
		out[name] = map[string]any{
			"rule": p.rule, "held": topics, "subscriptions": p.broker.SubscriberCount(),
			"topicCounts": p.broker.TopicCounts(), "last": l.last[name],
		}
	}
	return out
}

func (a *App) registerAdmit() {
	l := a.ops.admit
	a.mux.HandleFunc("GET /showcase/ops/admit", func(w http.ResponseWriter, _ *http.Request) {
		a.renderOpsPage(w, admitPage, map[string]any{"Title": "Admission limits", "Family": "admit", "Policies": l.order, "Topics": adTopics, "L": l})
	})
	reply := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, template.HTMLEscapeString(s))
	}
	policy := func(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Query().Get("policy")
			if l.policies[p] == nil {
				http.Error(w, "Unknown policy", http.StatusBadRequest)
				return
			}
			h(w, r, p)
		}
	}
	a.mux.HandleFunc("POST /showcase/ops/admit/add", policy(func(w http.ResponseWriter, r *http.Request, p string) {
		topic := r.URL.Query().Get("topic")
		if !slices.Contains(adTopics[p], topic) {
			http.Error(w, "Unknown topic", http.StatusBadRequest)
			return
		}
		reply(w, l.add(p, topic))
	}))
	a.mux.HandleFunc("POST /showcase/ops/admit/remove", policy(func(w http.ResponseWriter, _ *http.Request, p string) {
		reply(w, l.remove(p))
	}))
	a.mux.HandleFunc("POST /showcase/ops/admit/multi", func(w http.ResponseWriter, r *http.Request) {
		reply(w, l.multi(r.URL.Query().Get("strict") == "1"))
	})
	// A stream request to SSEHandler on the global broker, served to a
	// recorder for a moment: it answers 503 when the broker is full.
	stream := l.policies["global"].broker.SSEHandler("ops:admit:a")
	a.mux.HandleFunc("POST /showcase/ops/admit/stream", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		rec := httptest.NewRecorder()
		stream.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/stream", http.NoBody))
		msg := fmt.Sprintf("%d %s", rec.Code, http.StatusText(rec.Code))
		if rec.Code == http.StatusOK {
			msg += ": the stream opened (closed after 100 ms)"
		}
		l.log.add("stream request: %s", msg)
		reply(w, msg)
	})
	a.mux.HandleFunc("GET /showcase/ops/admit/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, l.state())
	})
}

var admitPage = deliveryPage("admit", `{{define "main"}}
<p class="note">Each broker below has one kind of limit. The buttons add or remove subscriptions this page holds on the server (at most six per broker), so you can fill a limit, see the refusal and its reason, and make room again. A refused subscription changes nothing that was already admitted. A subscription to both topics at once skips a topic that is full and keeps the other; the all-or-nothing form is refused as a whole instead, undoing the topic it had already joined. Limits are shared by every window.</p>
{{range $p := .Policies}}
<section class="admit-policy" data-policy="{{$p}}">
  <h2>{{$p}}</h2>
  <p>
    {{range index $.Topics $p}}<button hx-post="/showcase/ops/admit/add?policy={{$p}}&amp;topic={{.}}" hx-target="#admit-{{$p}}">Subscribe to {{.}}</button> {{end}}
    <button hx-post="/showcase/ops/admit/remove?policy={{$p}}" hx-target="#admit-{{$p}}">Remove the oldest</button>
    {{if eq $p "topic"}}<button hx-post="/showcase/ops/admit/multi" hx-target="#admit-{{$p}}">Subscribe to both at once</button>
    <button hx-post="/showcase/ops/admit/multi?strict=1" hx-target="#admit-{{$p}}">Both at once, all or nothing</button>{{end}}
  </p>
  <p>Result: <output id="admit-{{$p}}"></output></p>
</section>
{{end}}
<section>
  <h2>A stream request</h2>
  <p><button hx-post="/showcase/ops/admit/stream" hx-target="#admit-stream">Request a stream from the global broker</button> <output id="admit-stream"></output></p>
  <p class="note">The server sends a stream request to SSEHandler on the global broker and shows its status. When the broker is full it answers 503 Service Unavailable; otherwise the stream opens and is closed after 100 ms.</p>
</section>
{{end}}`)
