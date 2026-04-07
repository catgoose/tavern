package tavern

import (
	"context"
	"sync"
	"time"
)

// ReplayStore is an abstraction for storing and retrieving replay entries.
// Implementations must be safe for concurrent use by multiple goroutines.
// The default in-memory implementation is [MemoryReplayStore].
//
// IDs are topic-scoped (not global). TTL filtering happens at read time:
// stores must not return expired entries from [ReplayStore.AfterID] or
// [ReplayStore.Latest].
type ReplayStore interface {
	// Append stores a replay entry for a topic.
	Append(ctx context.Context, topic string, entry ReplayEntry) error

	// AfterID returns entries published after the given ID.
	// found indicates whether lastID exists in the store.
	// If found=false, the broker treats this as a gap.
	// Must not return expired entries (TTL filtering at read time).
	AfterID(ctx context.Context, topic, lastID string, limit int) (entries []ReplayEntry, found bool, err error)

	// Latest returns the most recent entries for a topic (for initial
	// subscribe replay). Must not return expired entries.
	Latest(ctx context.Context, topic string, limit int) ([]ReplayEntry, error)

	// DeleteTopic removes all replay entries for a topic.
	DeleteTopic(ctx context.Context, topic string) error

	// SetMaxEntries configures the maximum number of entries to retain for a
	// topic. When the limit is exceeded, the oldest entries are discarded.
	// A value of 0 or less removes all entries and the limit for the topic.
	SetMaxEntries(ctx context.Context, topic string, n int) error
}

// MemoryReplayStore is an in-memory implementation of [ReplayStore]. It stores
// entries in ordered slices per topic with a configurable maximum size.
// All methods are safe for concurrent use.
type MemoryReplayStore struct {
	mu       sync.Mutex
	entries  map[string][]ReplayEntry
	maxSize  map[string]int
}

// NewMemoryReplayStore creates a ready-to-use in-memory replay store.
func NewMemoryReplayStore() *MemoryReplayStore {
	return &MemoryReplayStore{
		entries: make(map[string][]ReplayEntry),
		maxSize: make(map[string]int),
	}
}

// Append stores a replay entry for a topic. If the number of entries exceeds
// the configured maximum for the topic, the oldest entries are discarded.
// The default maximum is 1 if not explicitly configured via [MemoryReplayStore.SetMaxEntries].
func (m *MemoryReplayStore) Append(_ context.Context, topic string, entry ReplayEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	maxSize := 1
	if n, ok := m.maxSize[topic]; ok {
		maxSize = n
	}

	log := m.entries[topic]
	log = append(log, entry)
	if len(log) > maxSize {
		log = log[len(log)-maxSize:]
	}
	m.entries[topic] = log
	return nil
}

// AfterID returns entries published after the given ID. If lastID is not found
// in the store, found is false and entries is nil (indicating a gap). Expired
// entries are filtered out.
func (m *MemoryReplayStore) AfterID(_ context.Context, topic, lastID string, _ int) ([]ReplayEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := m.entries[topic]
	now := time.Now()

	for i, entry := range log {
		if entry.ID == lastID {
			var result []ReplayEntry
			for _, e := range log[i+1:] {
				if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
					continue
				}
				result = append(result, e)
			}
			return result, true, nil
		}
	}
	return nil, false, nil
}

// Latest returns the most recent entries for a topic, up to limit. Expired
// entries are filtered out.
func (m *MemoryReplayStore) Latest(_ context.Context, topic string, limit int) ([]ReplayEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	log := m.entries[topic]
	if len(log) == 0 {
		return nil, nil
	}

	now := time.Now()
	var result []ReplayEntry
	// Walk from newest to oldest, collecting up to limit non-expired entries.
	for i := len(log) - 1; i >= 0 && len(result) < limit; i-- {
		e := log[i]
		if !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt) {
			continue
		}
		result = append(result, e)
	}
	// Reverse to restore chronological order.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result, nil
}

// DeleteTopic removes all replay entries for a topic and its max-size config.
func (m *MemoryReplayStore) DeleteTopic(_ context.Context, topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.entries, topic)
	delete(m.maxSize, topic)
	return nil
}

// SetMaxEntries configures the maximum number of entries to retain for a topic.
// If n <= 0, all entries and the limit are removed (equivalent to DeleteTopic).
// If the current number of entries exceeds n, the oldest are discarded.
func (m *MemoryReplayStore) SetMaxEntries(_ context.Context, topic string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if n <= 0 {
		delete(m.entries, topic)
		delete(m.maxSize, topic)
		return nil
	}

	m.maxSize[topic] = n
	if log := m.entries[topic]; len(log) > n {
		m.entries[topic] = log[len(log)-n:]
	}
	return nil
}
