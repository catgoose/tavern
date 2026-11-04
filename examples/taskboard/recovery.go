package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/catgoose/tavern"
)

// The recovery lab runs each scenario on its own broker, so broker-wide
// settings (buffer size, drop policy, eviction, replay store) stay with the
// scenario and never touch the rest of the app. Every scenario has one shared
// canonical value per topic; the controls that cause a loss act on one
// window's connection only.
const (
	recoveryRetention   = 5 // replay entries kept per topic
	recoveryLogLimit    = 20
	recoveryAwayMax     = 20
	recoverySmallBuffer = 4
)

// recoveryKind is how a scenario subscribes.
type recoveryKind int

const (
	kindResume      recoveryKind = iota // SubscribeFromIDWith on one topic
	kindMulti                           // SubscribeMultiFromID on two topics
	kindPressure                        // SubscribeWith with a snapshot; plain publishes
	kindStore                           // SubscribeFromIDWith backed by a ReplayStore
	kindStoreStrict                     // the same with SubWithTerminateOnReplayError
)

type scenario struct {
	name, title, about string
	kind               recoveryKind
	broker             *tavern.SSEBroker
	topics             []string
	lossAware          bool
	retention          int // replay entries kept per topic; recoveryRetention when zero

	// pubMu orders publications against subscription setup: it is held
	// across every publish and across each subscribe, including its replay
	// and gap snapshot, so no change can be queued ahead of a snapshot
	// older than it. Lock order is pubMu, then mu; gap snapshot callbacks,
	// which the broker calls during a subscribe, take only mu.
	pubMu  sync.Mutex
	mu     sync.Mutex
	values map[string]int // topic → canonical value
	seq    int            // last event ID, shared by the scenario's topics

	connMu sync.Mutex
	conns  map[string]*recoveryConn // connection id → its open stream's owner

	// The log has its own lock: replay store errors are reported while a
	// publish holds mu.
	logMu sync.Mutex
	log   []observation // newest first, bounded

	holds  holds
	repair feedRepair
	store  *faultyStore
}

func (s *scenario) Name() string  { return s.name }
func (s *scenario) Title() string { return s.title }
func (s *scenario) About() string { return s.about }

// observation is one thing the server saw, for one window or for everyone.
type observation struct {
	Conn string `json:"conn,omitempty"`
	Text string `json:"text"`
}

func (s *scenario) observe(conn, format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.log = append([]observation{{Conn: conn, Text: fmt.Sprintf(format, args...)}}, s.log...)
	if len(s.log) > recoveryLogLimit {
		s.log = s.log[:recoveryLogLimit]
	}
}

// change adds one to topic's value and publishes every topic's current value
// with one new event ID, so a multi-topic resume finds the ID in each log.
func (s *scenario) change(topic string) {
	s.publish(func() { s.values[topic]++ })
}

// publish applies update, if any, and publishes the current values.
func (s *scenario) publish(update func()) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if update != nil {
		update()
	}
	s.publishLocked()
}

func (s *scenario) publishLocked() {
	s.seq++
	id := strconv.Itoa(s.seq)
	for _, t := range s.topics {
		msg := s.valueMessage(t)
		if s.kind == kindPressure {
			s.broker.Publish(t, msg)
		} else {
			s.broker.PublishWithID(t, id, msg)
		}
	}
}

func (s *scenario) valueMessage(topic string) string {
	return tavern.NewSSEMessage(eventFor(topic), strconv.Itoa(s.values[topic])).String()
}

// snapshotLocked is a topic's current value. For ID-backed topics it carries
// the latest event ID, so the window's next resume starts from it.
func (s *scenario) snapshotLocked(topic string) string {
	m := tavern.NewSSEMessage(eventFor(topic), strconv.Itoa(s.values[topic]))
	if s.kind != kindPressure {
		m = m.WithID(strconv.Itoa(s.seq))
	}
	return m.String()
}

func eventFor(topic string) string {
	switch topic {
	case "recovery/left":
		return "left"
	case "recovery/right":
		return "right"
	}
	return "value"
}

func (s *scenario) current() map[string]int {
	values, _ := s.state()
	return values
}

// state is the current values and the event ID they were published with.
func (s *scenario) state() (values map[string]int, rev int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values = make(map[string]int, len(s.values))
	for k, v := range s.values {
		values[k] = v
	}
	return values, s.seq
}

func (s *scenario) observations(conn string) []observation {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	out := []observation{}
	for _, o := range s.log {
		if o.Conn == "" || o.Conn == conn {
			out = append(out, o)
		}
	}
	return out
}

// applyPolicies sets the scenario's replay retention, gap policy and
// bundling. ClearReplay removes all three, so it is applied again after.
func (s *scenario) applyPolicies() {
	for _, t := range s.topics {
		if s.kind == kindPressure {
			continue
		}
		retention := s.retention
		if retention == 0 {
			retention = recoveryRetention
		}
		s.broker.SetReplayPolicy(t, retention)
		strategy := tavern.GapFallbackToSnapshot
		if s.name == "resume-silent" {
			strategy = tavern.GapSilent
		}
		topic := t
		s.broker.SetReplayGapPolicy(t, strategy, func() string {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.snapshotLocked(topic)
		})
	}
	if s.kind == kindMulti {
		s.broker.SetBundleOnReconnect("recovery/left", true)
	}
}

// clearReplay drops the scenario's retained history (in the store, for the
// store scenarios) and applies its policies again.
func (s *scenario) clearReplay() {
	for _, t := range s.topics {
		s.broker.ClearReplay(t)
	}
	s.applyPolicies()
	s.observe("", "replay history cleared")
}

// faultyStore is a MemoryReplayStore whose appends and reads can be made to
// fail, to show what the broker does when its replay store does.
type faultyStore struct {
	*tavern.MemoryReplayStore
	failAppend atomic.Bool
	failRead   atomic.Bool
}

var errStoreDown = errors.New("replay store unavailable")

func (f *faultyStore) Append(ctx context.Context, topic string, e tavern.ReplayEntry) error {
	if f.failAppend.Load() {
		return errStoreDown
	}
	return f.MemoryReplayStore.Append(ctx, topic, e)
}

func (f *faultyStore) AfterID(ctx context.Context, topic, lastID string, limit int) ([]tavern.ReplayEntry, bool, error) {
	if f.failRead.Load() {
		return nil, false, errStoreDown
	}
	return f.MemoryReplayStore.AfterID(ctx, topic, lastID, limit)
}

func (f *faultyStore) Latest(ctx context.Context, topic string, limit int) ([]tavern.ReplayEntry, error) {
	if f.failRead.Load() {
		return nil, errStoreDown
	}
	return f.MemoryReplayStore.Latest(ctx, topic, limit)
}

// recovery holds the lab's scenarios and their brokers.
type recovery struct {
	scenarios map[string]*scenario
	order     []*scenario
	brokers   []*tavern.SSEBroker
}

func newRecovery() *recovery {
	r := &recovery{scenarios: map[string]*scenario{}}
	replay := tavern.NewSSEBroker(tavern.WithBufferSize(32))
	truncating := tavern.NewSSEBroker(tavern.WithBufferSize(recoverySmallBuffer))
	newest := tavern.NewSSEBroker(tavern.WithBufferSize(recoverySmallBuffer))
	oldest := tavern.NewSSEBroker(tavern.WithBufferSize(recoverySmallBuffer), tavern.WithDropOldest())
	evicting := tavern.NewSSEBroker(tavern.WithBufferSize(recoverySmallBuffer), tavern.WithSlowSubscriberEviction(3))
	store := &faultyStore{MemoryReplayStore: tavern.NewMemoryReplayStore()}
	var stored []*scenario
	storeBroker := tavern.NewSSEBroker(tavern.WithBufferSize(32), tavern.WithReplayStore(store),
		tavern.WithReplayStoreErrorHandler(func(e *tavern.ReplayStoreError) {
			for _, s := range stored {
				if slices.Contains(s.topics, e.Topic) {
					s.observe("", "replay store %s on %s failed: %v", e.Op, e.Topic, e.Err)
				}
			}
		}))
	r.brokers = []*tavern.SSEBroker{replay, truncating, newest, oldest, evicting, storeBroker}

	add := func(s *scenario) *scenario {
		s.values = map[string]int{}
		s.conns = map[string]*recoveryConn{}
		s.holds.ttl, s.holds.max = holdTTL, holdMax
		r.scenarios[s.name] = s
		r.order = append(r.order, s)
		return s
	}
	add(&scenario{name: "resume", title: "Resume with Last-Event-ID", kind: kindResume, broker: replay, topics: []string{"recovery/resume"},
		about: "ID-backed replay keeps the last five values. A short absence is replayed; a longer one is a gap, answered with the gap event and a fresh snapshot (GapFallbackToSnapshot)."})
	add(&scenario{name: "resume-silent", title: "Gap without a snapshot", kind: kindResume, broker: replay, topics: []string{"recovery/silent"},
		about: "The same replay with GapSilent: after a long absence the server skips the missing history without telling the window, so it shows an old value until the next change. Only the server sees the gap."})
	add(&scenario{name: "multi", title: "Two topics on one resume", kind: kindMulti, broker: replay, topics: []string{"recovery/left", "recovery/right"},
		about: "SubscribeMultiFromID resumes both topics from the one Last-Event-ID. Every change publishes both values with the same ID, so the ID is in both logs. Left's replay is bundled into one write; right's is not. Each topic replays, and reports gaps, on its own."})
	trunc := add(&scenario{name: "truncation", title: "Replay larger than the queue", kind: kindResume, broker: truncating, topics: []string{"recovery/truncated"}, retention: 16,
		about: "This broker's queues hold four messages and its replay keeps sixteen, so an absence of more than four changes replays more than fits. The rest are dropped, and so are the reconnected and truncated notices queued after them, so tavern-js never gets the confirmation and keeps the region in Reconnecting. The server sees the counts in OnReconnect and republishes the current value until it is delivered. Reconnect now resumes with nothing missed, which the server does confirm."})
	add(&scenario{name: "drop-newest", title: "Full queue, drop newest", kind: kindPressure, broker: newest, topics: []string{"recovery/newest"},
		about: "The default: a full queue drops the newest message, so the final value is the one lost. The server republishes the current value after a drop until one is delivered."})
	add(&scenario{name: "drop-oldest", title: "Full queue, drop oldest", kind: kindPressure, broker: oldest, topics: []string{"recovery/oldest"},
		about: "WithDropOldest: a full queue discards its oldest message, so the newest values, including the last, are delivered."})
	add(&scenario{name: "evict", title: "Evict a slow window", kind: kindPressure, broker: evicting, topics: []string{"recovery/evicted"},
		about: "WithSlowSubscriberEviction(3): after three drops in a row the window's subscription is closed. The browser reconnects and starts from a snapshot."})
	add(&scenario{name: "loss-aware", title: "End the stream on loss", kind: kindPressure, broker: newest, topics: []string{"recovery/lossaware"}, lossAware: true,
		about: "SubWithDisconnectOnLoss: the first drop ends the subscription with reason queue-loss. The browser reconnects and starts from a snapshot."})
	stored = append(stored,
		add(&scenario{name: "store", title: "Replay store failures", kind: kindStore, broker: storeBroker, topics: []string{"recovery/stored"}, store: store,
			about: "Replay is kept in a ReplayStore (an in-memory one, so it does not survive a restart). The store can be made to fail. A failed read is reported and treated as a gap, so the window gets the gap event and a snapshot. Failed appends are lost without a gap: the resume looks complete and the window stays behind. Repair the store, clear the replay history and reconnect, and the resume is a gap answered with a snapshot."}),
		add(&scenario{name: "store-strict", title: "Replay store failures, strict", kind: kindStoreStrict, broker: storeBroker, topics: []string{"recovery/strict"}, store: store,
			about: "The same store with SubWithTerminateOnReplayError: a failed read ends the subscription with reason replay-failure instead. The app then serves a snapshot subscription on the same response, so the window still ends up current. No reconnected notice is sent for a failed replay, so the region stays in Reconnecting until a resume succeeds: repair the store, then Reconnect now."}),
	)

	newest.OnPublishDrop(func(topic string, _ int) {
		if s := r.scenarios["drop-newest"]; s.topics[0] == topic {
			s.repair.onDrop(topic)
		}
	})
	truncating.OnPublishDrop(func(topic string, _ int) { trunc.repair.onDrop(topic) })
	for _, s := range r.order {
		s.applyPolicies()
		s.repair.init(func(topic string) bool { return slices.Contains(s.topics, topic) }, func() { s.publish(nil) })
		for _, t := range s.topics {
			topic := t
			if s.kind == kindPressure {
				continue
			}
			s.broker.OnReconnect(topic, func(info tavern.ReconnectInfo) {
				s.observe(info.SubscriberID, "%s: resumed after %s, %d missed, %d replayed, %d dropped",
					topic, info.LastEventID, info.MissedCount, info.ReplayDelivered, info.ReplayDropped)
				if info.ReplayDropped > 0 && s == trunc {
					s.repair.onDrop(topic) // republish until a value gets through
				}
			})
			s.broker.OnReplayGap(topic, func(sub *tavern.SubscriberInfo, lastID string) {
				conn := ""
				if sub != nil {
					conn = sub.ID
				}
				s.observe(conn, "%s: gap, event %s is no longer kept", topic, lastID)
			})
		}
		s.publish(nil) // the starting values
	}
	return r
}

func (r *recovery) close() {
	for _, s := range r.order {
		s.holds.close()
		s.repair.close()
	}
	for _, b := range r.brokers {
		b.Close()
	}
}

// subscribe opens one window's single-topic stream. A fresh connection
// (no Last-Event-ID) starts from a snapshot of the current value, with no
// replay: replaying every kept value into a small queue could drop the
// newest. A resume replays from the window's Last-Event-ID. Both hold pubMu
// until the subscription, its replay and any gap snapshot are queued.
func (s *scenario) subscribe(conn, lastID string, status *tavern.SubscriptionStatus) (msgs <-chan string, unsubscribe func()) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	topic := s.topics[0]
	opts := []tavern.SubscribeOption{tavern.SubWithMeta(tavern.SubscribeMeta{ID: conn}), tavern.SubWithStatus(status)}
	if s.kind == kindPressure || lastID == "" {
		if s.lossAware {
			opts = append(opts, tavern.SubWithDisconnectOnLoss())
		}
		snapshot := s.snapshot(topic)
		return s.broker.SubscribeWith(topic, append(opts, tavern.SubWithSnapshot(func() string { return snapshot }))...)
	}
	if s.kind == kindStoreStrict {
		opts = append(opts, tavern.SubWithTerminateOnReplayError())
	}
	return s.broker.SubscribeFromIDWith(topic, lastID, opts...)
}

// subscribeMulti resumes every topic from one Last-Event-ID, under pubMu.
func (s *scenario) subscribeMulti(lastID string) (msgs <-chan tavern.TopicMessage, unsubscribe func()) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	return s.broker.SubscribeMultiFromID(s.topics, lastID)
}

// fallbackSubscribe is the strict store scenario's way back after a failed
// replay: a snapshot subscription, under pubMu.
func (s *scenario) fallbackSubscribe() (msgs <-chan string, unsubscribe func()) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	topic := s.topics[0]
	snapshot := s.snapshot(topic)
	return s.broker.SubscribeWithSnapshot(topic, func() string { return snapshot })
}

func (s *scenario) snapshot(topic string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(topic)
}

// recoveryConn owns one window's open stream: its pause gate and the means
// to end it. A connection ID has at most one owner at a time.
type recoveryConn struct {
	gate        *pauser
	done        chan struct{} // closed once the stream is torn down and released
	unsubscribe func()        // set under connMu once subscribed
}

const recoveryMaxOpen = 32 // open streams per scenario

var (
	errConnInUse = errors.New("this connection already has an open stream")
	errConnsFull = errors.New("too many open streams")
)

// reserve makes c the owner of conn, unless conn is already owned.
func (s *scenario) reserve(conn string, c *recoveryConn) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if _, ok := s.conns[conn]; ok {
		return errConnInUse
	}
	if len(s.conns) >= recoveryMaxOpen {
		return errConnsFull
	}
	s.conns[conn] = c
	return nil
}

// release ends c's ownership of conn; a later owner is left alone.
func (s *scenario) release(conn string, c *recoveryConn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conns[conn] == c {
		delete(s.conns, conn)
	}
}

// owner is conn's current owner, if any.
func (s *scenario) owner(conn string) *recoveryConn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conns[conn]
}

func (s *scenario) setUnsubscribe(c *recoveryConn, unsub func()) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	c.unsubscribe = unsub
}

// subscribed is conn's owner and its unsubscribe, once it has subscribed.
func (s *scenario) subscribed(conn string) (owner *recoveryConn, unsubscribe func()) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	c := s.conns[conn]
	if c == nil || c.unsubscribe == nil {
		return nil, nil
	}
	return c, c.unsubscribe
}

func (s *scenario) openStreams() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}
