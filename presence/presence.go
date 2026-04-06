// Package presence provides structured presence tracking built on top of a
// tavern SSE broker. It manages per-topic user presence with heartbeat-based
// stale detection and optional OOB publishing for real-time UI updates.
//
// The package imports tavern (not the other way around) so there is no circular
// dependency. Presence is opt-in — the core broker knows nothing about it.
package presence

import (
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

const (
	// DefaultStaleTimeout is the default duration after which a user with no
	// heartbeat is considered stale and removed.
	DefaultStaleTimeout = 30 * time.Second

	// DefaultPresenceTopicSuffix is appended to the base topic when
	// publishing OOB presence updates.
	DefaultPresenceTopicSuffix = ":presence"

	// DefaultTargetID is the default OOB swap target element ID.
	DefaultTargetID = "presence-list"
)

// RenderFunc converts the current presence list for a topic into an HTML
// string suitable for OOB publishing. If nil, the tracker operates in
// server-side-only mode (no OOB updates are published).
type RenderFunc func(topic string, users []Info) string

// Config configures a presence [Tracker].
type Config struct {
	// StaleTimeout is how long since the last heartbeat before a user is
	// removed. Zero or negative values use [DefaultStaleTimeout].
	StaleTimeout time.Duration

	// OnJoin fires when a user joins a topic.
	OnJoin func(topic string, info Info)
	// OnLeave fires when a user leaves a topic (explicit or stale).
	OnLeave func(topic string, info Info)
	// OnUpdate fires when a user's presence metadata is updated.
	OnUpdate func(topic string, info Info)

	// RenderFunc converts presence state to HTML for OOB publishing.
	// When nil, no OOB updates are published.
	RenderFunc RenderFunc

	// PresenceTopicSuffix is appended to the base topic for OOB publishes.
	// Defaults to [DefaultPresenceTopicSuffix].
	PresenceTopicSuffix string

	// TargetID is the element ID used in OOB swap fragments.
	// Defaults to [DefaultTargetID].
	TargetID string
}

// Info represents a user's presence on a topic. All fields except UserID are
// optional. Metadata can hold arbitrary application-specific data such as
// cursor positions, status text, or editing state.
type Info struct {
	UserID   string
	Name     string
	Avatar   string
	Metadata map[string]any // arbitrary fields (cursor position, status, etc.)
	JoinedAt time.Time
	LastSeen time.Time
}

// Tracker manages presence state for topics. It is safe for concurrent use
// by multiple goroutines. A background goroutine sweeps stale entries at half
// the configured StaleTimeout interval.
type Tracker struct {
	broker *tavern.SSEBroker
	cfg    Config

	mu     sync.RWMutex
	topics map[string]map[string]*Info // topic → userID → *Info

	done chan struct{}
	once sync.Once // ensures Close only runs once
}

// New creates a presence tracker backed by a tavern broker. The background
// stale-checker goroutine starts immediately and runs until [Tracker.Close]
// is called.
func New(broker *tavern.SSEBroker, cfg Config) *Tracker {
	if cfg.StaleTimeout <= 0 {
		cfg.StaleTimeout = DefaultStaleTimeout
	}
	if cfg.PresenceTopicSuffix == "" {
		cfg.PresenceTopicSuffix = DefaultPresenceTopicSuffix
	}
	if cfg.TargetID == "" {
		cfg.TargetID = DefaultTargetID
	}

	t := &Tracker{
		broker: broker,
		cfg:    cfg,
		topics: make(map[string]map[string]*Info),
		done:   make(chan struct{}),
	}
	go t.sweepLoop()
	return t
}

// Join registers presence on a topic. If the user is already present the call
// is a no-op (use [Tracker.Update] to change metadata).
func (t *Tracker) Join(topic string, info Info) {
	now := time.Now()
	t.mu.Lock()
	users := t.topics[topic]
	if users == nil {
		users = make(map[string]*Info)
		t.topics[topic] = users
	}
	if _, exists := users[info.UserID]; exists {
		t.mu.Unlock()
		return
	}
	info.JoinedAt = now
	info.LastSeen = now
	// Deep-copy metadata so the caller can't mutate internal state.
	if info.Metadata != nil {
		cp := make(map[string]any, len(info.Metadata))
		for k, v := range info.Metadata {
			cp[k] = v
		}
		info.Metadata = cp
	}
	users[info.UserID] = &info
	snapshot := t.snapshotLocked(topic)
	t.mu.Unlock()

	if t.cfg.OnJoin != nil {
		t.cfg.OnJoin(topic, info)
	}
	t.publishOOB(topic, snapshot)
}

// Leave removes presence from a topic.
func (t *Tracker) Leave(topic, userID string) {
	t.mu.Lock()
	users := t.topics[topic]
	if users == nil {
		t.mu.Unlock()
		return
	}
	info, ok := users[userID]
	if !ok {
		t.mu.Unlock()
		return
	}
	cp := *info
	delete(users, userID)
	if len(users) == 0 {
		delete(t.topics, topic)
	}
	snapshot := t.snapshotLocked(topic)
	t.mu.Unlock()

	if t.cfg.OnLeave != nil {
		t.cfg.OnLeave(topic, cp)
	}
	t.publishOOB(topic, snapshot)
}

// Update merges partial presence state. Only non-zero fields in metadata are
// merged — existing keys not present in the update are preserved. Name and
// Avatar are not changed by Update; use Join for initial values.
func (t *Tracker) Update(topic, userID string, metadata map[string]any) {
	t.mu.Lock()
	users := t.topics[topic]
	if users == nil {
		t.mu.Unlock()
		return
	}
	info, ok := users[userID]
	if !ok {
		t.mu.Unlock()
		return
	}
	if info.Metadata == nil {
		info.Metadata = make(map[string]any)
	}
	for k, v := range metadata {
		info.Metadata[k] = v
	}
	info.LastSeen = time.Now()
	cp := *info
	if info.Metadata != nil {
		cp.Metadata = make(map[string]any, len(info.Metadata))
		for k, v := range info.Metadata {
			cp.Metadata[k] = v
		}
	}
	snapshot := t.snapshotLocked(topic)
	t.mu.Unlock()

	if t.cfg.OnUpdate != nil {
		t.cfg.OnUpdate(topic, cp)
	}
	t.publishOOB(topic, snapshot)
}

// Heartbeat refreshes a user's LastSeen time without changing any other state.
func (t *Tracker) Heartbeat(topic, userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	users := t.topics[topic]
	if users == nil {
		return
	}
	if info, ok := users[userID]; ok {
		info.LastSeen = time.Now()
	}
}

// List returns a snapshot of all current presence entries for a topic.
// The returned slice is a copy and safe to use without synchronization.
func (t *Tracker) List(topic string) []Info {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snapshotLocked(topic)
}

// Get returns a specific user's presence info. The second return value is
// false if the user is not present on the topic.
func (t *Tracker) Get(topic, userID string) (Info, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	users := t.topics[topic]
	if users == nil {
		return Info{}, false
	}
	info, ok := users[userID]
	if !ok {
		return Info{}, false
	}
	cp := *info
	if info.Metadata != nil {
		cp.Metadata = make(map[string]any, len(info.Metadata))
		for k, v := range info.Metadata {
			cp.Metadata[k] = v
		}
	}
	return cp, true
}

// Close stops the stale checker goroutine and cleans up. It is safe to call
// multiple times.
func (t *Tracker) Close() {
	t.once.Do(func() {
		close(t.done)
	})
}

// snapshotLocked returns a copy of all presence entries for a topic. The
// caller must hold at least a read lock on t.mu.
func (t *Tracker) snapshotLocked(topic string) []Info {
	users := t.topics[topic]
	if len(users) == 0 {
		return nil
	}
	result := make([]Info, 0, len(users))
	for _, info := range users {
		cp := *info
		if info.Metadata != nil {
			cp.Metadata = make(map[string]any, len(info.Metadata))
			for k, v := range info.Metadata {
				cp.Metadata[k] = v
			}
		}
		result = append(result, cp)
	}
	return result
}

// publishOOB publishes presence state as an OOB fragment via the broker.
// If no RenderFunc is configured this is a no-op.
func (t *Tracker) publishOOB(topic string, users []Info) {
	if t.cfg.RenderFunc == nil {
		return
	}
	html := t.cfg.RenderFunc(topic, users)
	presenceTopic := topic + t.cfg.PresenceTopicSuffix
	t.broker.PublishOOB(presenceTopic, tavern.Replace(t.cfg.TargetID, html))
}

// sweepLoop periodically checks for stale presence entries.
func (t *Tracker) sweepLoop() {
	interval := t.cfg.StaleTimeout / 2
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.sweep()
		}
	}
}

// sweep removes stale entries from all topics.
func (t *Tracker) sweep() {
	now := time.Now()
	type staleEntry struct {
		topic    string
		info     Info
		snapshot []Info
	}

	var stale []staleEntry

	t.mu.Lock()
	for topic, users := range t.topics {
		for userID, info := range users {
			if now.Sub(info.LastSeen) > t.cfg.StaleTimeout {
				cp := *info
				if info.Metadata != nil {
					cp.Metadata = make(map[string]any, len(info.Metadata))
					for k, v := range info.Metadata {
						cp.Metadata[k] = v
					}
				}
				delete(users, userID)
				stale = append(stale, staleEntry{topic: topic, info: cp})
			}
		}
		if len(users) == 0 {
			delete(t.topics, topic)
		}
	}
	// Build snapshots after all removals for accurate state.
	for i := range stale {
		stale[i].snapshot = t.snapshotLocked(stale[i].topic)
	}
	t.mu.Unlock()

	for _, s := range stale {
		if t.cfg.OnLeave != nil {
			t.cfg.OnLeave(s.topic, s.info)
		}
		t.publishOOB(s.topic, s.snapshot)
	}
}
