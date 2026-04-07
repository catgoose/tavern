package tavern

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// groupDef is a static topic group definition.
type groupDef struct {
	topics []string
}

// dynamicGroupDef is a dynamic topic group evaluated per-request.
type dynamicGroupDef struct {
	fn func(r *http.Request) []string
}

// groupHandler serves a multiplexed SSE stream for a set of topics.
type groupHandler struct {
	broker          *SSEBroker
	topics          []string
	writer          SSEWriterFunc
	maxConnDuration time.Duration
}

// dynamicGroupHandler serves a multiplexed SSE stream for topics resolved per-request.
type dynamicGroupHandler struct {
	broker          *SSEBroker
	fn              func(r *http.Request) []string
	writer          SSEWriterFunc
	maxConnDuration time.Duration
}

// DefineGroup registers a named static topic group. The group can later be
// served via [SSEBroker.GroupHandler]. Defining a group with the same name
// again replaces the previous definition.
func (b *SSEBroker) DefineGroup(name string, topics []string) {
	b.groupsMu.Lock()
	defer b.groupsMu.Unlock()
	cp := make([]string, len(topics))
	copy(cp, topics)
	b.groups[name] = &groupDef{topics: cp}
}

// GroupHandler returns an [http.Handler] that streams SSE messages for all
// topics in the named static group. Each message is formatted with the topic
// as the SSE event type and the published data as the data field.
//
// GroupHandler accepts the same [SSEHandlerOption] values as [SSEBroker.SSEHandler].
// If the group name has not been defined, the handler responds with 404.
func (b *SSEBroker) GroupHandler(name string, opts ...SSEHandlerOption) http.Handler {
	b.groupsMu.RLock()
	g, ok := b.groups[name]
	b.groupsMu.RUnlock()
	if !ok {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("group %q not defined", name), http.StatusNotFound)
		})
	}
	h := &groupHandler{
		broker: b,
		topics: g.topics,
		writer: defaultSSEWriter,
	}
	for _, opt := range opts {
		adapter := &sseHandler{}
		opt(adapter)
		if adapter.writer != nil {
			h.writer = adapter.writer
		}
		if adapter.maxConnDuration > 0 {
			h.maxConnDuration = adapter.maxConnDuration
		}
	}
	return h
}

// DynamicGroup registers a named dynamic topic group. The provided function is
// evaluated per-request to determine which topics a given connection should
// subscribe to. This enables per-request authorization — different users can
// receive different topic sets from the same endpoint.
func (b *SSEBroker) DynamicGroup(name string, fn func(r *http.Request) []string) {
	b.dynGroupsMu.Lock()
	defer b.dynGroupsMu.Unlock()
	b.dynGroups[name] = &dynamicGroupDef{fn: fn}
}

// DynamicGroupHandler returns an [http.Handler] that streams SSE messages for
// topics determined per-request by the dynamic group's function. Each message
// is formatted with the topic as the SSE event type.
//
// DynamicGroupHandler accepts the same [SSEHandlerOption] values as
// [SSEBroker.SSEHandler]. If the group name has not been defined, the handler
// responds with 404.
func (b *SSEBroker) DynamicGroupHandler(name string, opts ...SSEHandlerOption) http.Handler {
	b.dynGroupsMu.RLock()
	g, ok := b.dynGroups[name]
	b.dynGroupsMu.RUnlock()
	if !ok {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("dynamic group %q not defined", name), http.StatusNotFound)
		})
	}
	h := &dynamicGroupHandler{
		broker: b,
		fn:     g.fn,
		writer: defaultSSEWriter,
	}
	for _, opt := range opts {
		adapter := &sseHandler{}
		opt(adapter)
		if adapter.writer != nil {
			h.writer = adapter.writer
		}
		if adapter.maxConnDuration > 0 {
			h.maxConnDuration = adapter.maxConnDuration
		}
	}
	return h
}

// ServeHTTP handles an incoming SSE connection for the static topic group.
func (h *groupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveGroupSSE(w, r, h.broker, h.topics, h.writer, h.maxConnDuration)
}

// ServeHTTP handles an incoming SSE connection for the dynamic topic group,
// resolving topics from the per-request function.
func (h *dynamicGroupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	topics := h.fn(r)
	serveGroupSSE(w, r, h.broker, topics, h.writer, h.maxConnDuration)
}

// serveGroupSSE is the shared streaming loop for static and dynamic group
// handlers. It subscribes to all topics via SubscribeMulti and writes each
// message as an SSE event with the topic as the event type. When the request
// includes a Last-Event-ID header, cached messages after that ID are replayed
// for each topic before the live stream begins.
func serveGroupSSE(w http.ResponseWriter, r *http.Request, broker *SSEBroker, topics []string, writer SSEWriterFunc, maxConnDuration time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	lastID := r.Header.Get("Last-Event-ID")

	var ch <-chan TopicMessage
	var unsub func()

	if lastID != "" {
		// When Last-Event-ID is present, use subscribeMultiFromID to replay
		// only messages after the given ID from each topic's replay log.
		ch, unsub = broker.subscribeMultiFromID(topics, lastID, writer, w)
		if ch == nil {
			return // writer error during replay
		}
	} else {
		ch, unsub = broker.SubscribeMulti(topics...)
	}
	defer unsub()

	var maxDurC <-chan time.Time
	if maxConnDuration > 0 {
		actual := maxConnDuration + time.Duration(rand.Int64N(int64(maxConnDuration/10)))
		maxDurC = time.After(actual)
	}

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return // broker closed
			}
			sseMsg := wrapForGroup(msg.Topic, msg.Data)
			if err := writer(w, sseMsg); err != nil {
				return // client disconnected
			}
		case <-r.Context().Done():
			return // client disconnected
		case <-maxDurC:
			writeMaxDurationClose(w)
			return
		}
	}
}

// subscribeMultiFromID subscribes to multiple topics using SubscribeFromID for
// each topic (with the same lastEventID), then replays the missed messages
// formatted as SSE events via the writer. It returns a merged channel and
// unsubscribe function like SubscribeMulti. If a write error occurs during
// replay, it returns nil for the channel.
func (b *SSEBroker) subscribeMultiFromID(topics []string, lastEventID string, writer SSEWriterFunc, w http.ResponseWriter) (msgs <-chan TopicMessage, unsubscribe func()) {
	out := make(chan TopicMessage, b.bufferSize)
	unsubs := make([]func(), 0, len(topics))
	done := make(chan struct{})
	var wg sync.WaitGroup

	// Collect per-topic channels via SubscribeFromID.
	perTopic := make([]<-chan string, len(topics))
	for i, topic := range topics {
		ch, unsub := b.SubscribeFromID(topic, lastEventID)
		unsubs = append(unsubs, unsub)
		perTopic[i] = ch
	}

	// Drain replay messages from each per-topic channel and write them
	// directly as SSE events. These are the messages that SubscribeFromID
	// buffered during subscribe.
	for i, topic := range topics {
		ch := perTopic[i]
	drainTopic:
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					break drainTopic
				}
				sseMsg := wrapForGroup(topic, msg)
				if err := writer(w, sseMsg); err != nil {
					// Clean up on write error.
					for _, unsub := range unsubs {
						unsub()
					}
					close(out)
					return nil, func() {}
				}
			default:
				break drainTopic
			}
		}
	}

	// Start fan-in goroutines for live messages.
	for i, topic := range topics {
		ch := perTopic[i]
		t := topic
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					select {
					case out <- TopicMessage{Topic: t, Data: msg}:
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	var closeOnce sync.Once
	closeOut := func() {
		closeOnce.Do(func() { close(out) })
	}

	var unsubOnce sync.Once
	doUnsub := func() {
		unsubOnce.Do(func() {
			close(done)
			for _, unsub := range unsubs {
				unsub()
			}
		})
	}

	go func() {
		wg.Wait()
		closeOut()
	}()

	return out, func() {
		doUnsub()
		wg.Wait()
		closeOut()
	}
}

// initGroupFields initializes the group-related maps on the broker.
func initGroupFields(b *SSEBroker) {
	b.groups = make(map[string]*groupDef)
	b.dynGroups = make(map[string]*dynamicGroupDef)
}
