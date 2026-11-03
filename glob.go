package tavern

import (
	"strings"
)

// globSub pairs a glob pattern with a subscriber channel and optional scope.
type globSub struct {
	pattern  string
	ch       chan TopicMessage
	scope    string // empty = unscoped
	segments []string
	closed   bool // guarded by SSEBroker.globMu
}

// closeLocked closes the subscriber channel once. The caller holds globMu
// for writing, which excludes dispatchToGlobSubscribers' sends.
func (s *globSub) closeLocked() {
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// removeGlobLocked unregisters s and closes its channel. The caller holds
// globMu for writing.
func (b *SSEBroker) removeGlobLocked(s *globSub) {
	for i, other := range b.globSubs {
		if other == s {
			b.globSubs = append(b.globSubs[:i], b.globSubs[i+1:]...)
			break
		}
	}
	s.closeLocked()
}

// SubscribeGlob registers a subscriber for all topics matching the given glob
// pattern and returns a channel that receives [TopicMessage] values tagged with
// the actual publish topic. The pattern uses "/" as the topic separator:
//   - "*" matches exactly one segment
//   - "**" matches zero or more segments (any depth)
//
// The returned unsubscribe function removes the glob subscriber and closes the
// channel. It is safe to call more than once.
func (b *SSEBroker) SubscribeGlob(pattern string) (msgs <-chan TopicMessage, unsubscribe func()) {
	return b.subscribeGlob(pattern, "")
}

// SubscribeGlobScoped registers a scoped glob subscriber. Only messages
// published via [SSEBroker.PublishTo] with a matching scope will be delivered.
func (b *SSEBroker) SubscribeGlobScoped(pattern, scope string) (msgs <-chan TopicMessage, unsubscribe func()) {
	return b.subscribeGlob(pattern, scope)
}

func (b *SSEBroker) subscribeGlob(pattern, scope string) (msgs <-chan TopicMessage, unsubscribe func()) {
	ch := make(chan TopicMessage, b.bufferSize)
	sub := &globSub{
		pattern:  pattern,
		ch:       ch,
		scope:    scope,
		segments: strings.Split(pattern, "/"),
	}

	b.globMu.Lock()
	if b.globClosed {
		b.globMu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.globSubs = append(b.globSubs, sub)
	b.globMu.Unlock()

	return ch, func() {
		b.globMu.Lock()
		b.removeGlobLocked(sub)
		b.globMu.Unlock()
	}
}

// UnsubscribeGlob is a convenience alias: callers may pass the channel
// returned by [SubscribeGlob] but the idiomatic approach is to call the
// unsubscribe function returned alongside the channel. This method finds and
// removes the glob subscription associated with ch, closing the channel.
func (b *SSEBroker) UnsubscribeGlob(ch <-chan TopicMessage) {
	b.globMu.Lock()
	defer b.globMu.Unlock()
	for _, s := range b.globSubs {
		if s.ch == ch {
			b.removeGlobLocked(s)
			return
		}
	}
}

// dispatchToGlobSubscribers sends msg (published to topic) to all glob
// subscribers whose pattern matches the topic. For scoped subscribers, the
// scope parameter must match; pass an empty string for unscoped publishes.
func (b *SSEBroker) dispatchToGlobSubscribers(topic, scope, msg string) {
	// Sends are non-blocking and happen under the read lock, so a glob
	// subscriber cannot be closed mid-send.
	b.globMu.RLock()
	defer b.globMu.RUnlock()
	if len(b.globSubs) == 0 {
		return
	}

	topicSegments := strings.Split(topic, "/")
	tm := TopicMessage{Topic: topic, Data: msg}

	for _, sub := range b.globSubs {
		// Scope filtering: unscoped glob subs receive unscoped publishes;
		// scoped glob subs only receive matching scoped publishes.
		if sub.scope != "" && sub.scope != scope {
			continue
		}
		// Unscoped glob subs should not receive scoped publishes.
		if sub.scope == "" && scope != "" {
			continue
		}
		if !matchGlob(sub.segments, topicSegments) {
			continue
		}
		select {
		case sub.ch <- tm:
		default:
			b.drops.Add(1)
		}
	}
}

// matchGlob reports whether topicSegments matches the pre-split pattern
// segments. "*" matches exactly one segment, "**" matches zero or more.
func matchGlob(pattern, topic []string) bool {
	return matchGlobRecursive(pattern, 0, topic, 0)
}

func matchGlobRecursive(pattern []string, pi int, topic []string, ti int) bool {
	for pi < len(pattern) {
		if pattern[pi] == "**" {
			// "**" matches zero or more segments.
			// Try matching the rest of the pattern against every suffix of topic.
			for k := ti; k <= len(topic); k++ {
				if matchGlobRecursive(pattern, pi+1, topic, k) {
					return true
				}
			}
			return false
		}
		if ti >= len(topic) {
			return false
		}
		if pattern[pi] == "*" {
			// "*" matches exactly one non-empty segment.
			if topic[ti] == "" {
				return false
			}
			pi++
			ti++
			continue
		}
		if pattern[pi] != topic[ti] {
			return false
		}
		pi++
		ti++
	}
	return ti == len(topic)
}
