package tavern

import (
	"fmt"
	"net/http"
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
	broker *SSEBroker
	topics []string
	writer SSEWriterFunc
}

// dynamicGroupHandler serves a multiplexed SSE stream for topics resolved per-request.
type dynamicGroupHandler struct {
	broker *SSEBroker
	fn     func(r *http.Request) []string
	writer SSEWriterFunc
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
	}
	return h
}

func (h *groupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveGroupSSE(w, r, h.broker, h.topics, h.writer)
}

func (h *dynamicGroupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	topics := h.fn(r)
	serveGroupSSE(w, r, h.broker, topics, h.writer)
}

// serveGroupSSE is the shared streaming loop for static and dynamic group
// handlers. It subscribes to all topics via SubscribeMulti and writes each
// message as an SSE event with the topic as the event type.
func serveGroupSSE(w http.ResponseWriter, r *http.Request, broker *SSEBroker, topics []string, writer SSEWriterFunc) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := broker.SubscribeMulti(topics...)
	defer unsub()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return // broker closed
			}
			sseMsg := NewSSEMessage(msg.Topic, msg.Data).String()
			if err := writer(w, sseMsg); err != nil {
				return // client disconnected
			}
		case <-r.Context().Done():
			return // client disconnected
		}
	}
}

// initGroupFields initializes the group-related maps on the broker.
func initGroupFields(b *SSEBroker) {
	b.groups = make(map[string]*groupDef)
	b.dynGroups = make(map[string]*dynamicGroupDef)
}
