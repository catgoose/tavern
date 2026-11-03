package tavern

import (
	"fmt"
	"net/http"
	"sync"
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
	handlerStream
}

// dynamicGroupHandler serves a multiplexed SSE stream for topics resolved per-request.
type dynamicGroupHandler struct {
	broker *SSEBroker
	fn     func(r *http.Request) []string
	handlerStream
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
	return &groupHandler{broker: b, topics: g.topics, handlerStream: handlerStreamFrom(opts)}
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
	return &dynamicGroupHandler{broker: b, fn: g.fn, handlerStream: handlerStreamFrom(opts)}
}

// ServeHTTP handles an incoming SSE connection for the static topic group.
func (h *groupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveGroup(w, r, h.broker, h.topics)
}

// ServeHTTP handles an incoming SSE connection for the dynamic topic group,
// resolving topics from the per-request function.
func (h *dynamicGroupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	topics := h.fn(r)
	h.serveGroup(w, r, h.broker, topics)
}

// serveGroup is the shared streaming loop for static and dynamic group
// handlers. It subscribes to all topics via SubscribeMulti and writes each
// message as an SSE event with the topic as the event type. When the request
// includes a Last-Event-ID header, cached messages after that ID are replayed
// for each topic before the live stream begins.
func (c *handlerStream) serveGroup(w http.ResponseWriter, r *http.Request, broker *SSEBroker, topics []string) {
	fw := c.begin(broker, w)
	if fw == nil {
		return
	}
	defer fw.releaseOnCancel(r.Context())()

	lastID := r.Header.Get("Last-Event-ID")

	var ch <-chan TopicMessage
	var unsub func()

	if lastID != "" {
		// When Last-Event-ID is present, use subscribeMultiFromID to replay
		// only messages after the given ID from each topic's replay log.
		var err error
		ch, unsub, err = broker.subscribeMultiFromID(topics, lastID, fw)
		if err != nil {
			c.report(broker, err)
			return
		}
	} else {
		ch, unsub = broker.SubscribeMulti(topics...)
	}
	defer unsub()

	maxDurC, stopMaxDur := c.maxDuration()
	defer stopMaxDur()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return // broker closed
			}
			if err := fw.frame(wrapForGroup(msg.Topic, msg.Data)); err != nil {
				c.report(broker, err)
				return
			}
		case <-r.Context().Done():
			return // client disconnected
		case <-maxDurC:
			c.closeForMaxDuration(broker, fw)
			return
		}
	}
}

// subscribeMultiFromID subscribes to multiple topics using SubscribeFromID for
// each topic (with the same lastEventID), then replays the missed messages
// formatted as SSE events through fw. It returns a merged channel and
// unsubscribe function like SubscribeMulti. If a replay write fails, it
// unsubscribes and returns the error.
func (b *SSEBroker) subscribeMultiFromID(topics []string, lastEventID string, fw *frameWriter) (msgs <-chan TopicMessage, unsubscribe func(), err error) {
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
				if err := fw.frame(wrapForGroup(topic, msg)); err != nil {
					for _, unsub := range unsubs {
						unsub()
					}
					return nil, nil, err
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
	}, nil
}

// initGroupFields initializes the group-related maps on the broker.
func initGroupFields(b *SSEBroker) {
	b.groups = make(map[string]*groupDef)
	b.dynGroups = make(map[string]*dynamicGroupDef)
}
