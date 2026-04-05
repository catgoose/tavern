package tavern

import "strings"

// PublishFunc is the function signature for publish operations.
// Middleware wraps this to intercept, transform, or swallow publishes.
type PublishFunc func(topic, msg string)

// Middleware wraps a [PublishFunc] to add cross-cutting behaviour to the
// publish pipeline.  Middleware is called in registration order (first
// registered = outermost) and may transform the message, add side-effects,
// or swallow the publish entirely by not calling next.
type Middleware func(next PublishFunc) PublishFunc

// topicMiddleware pairs a pattern with a middleware function.
type topicMiddleware struct {
	pattern    string
	middleware Middleware
}

// Use registers global middleware that runs on every publish regardless of
// topic.  Middleware executes in registration order (first registered =
// outermost wrapper).  It must be called before any publishes; adding
// middleware while publishing is safe but the new middleware only takes
// effect on subsequent publishes.
func (b *SSEBroker) Use(mw Middleware) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.middlewares = append(b.middlewares, mw)
}

// UseTopics registers middleware that runs only when the topic matches
// the given pattern.  Pattern matching uses simple wildcard rules:
//   - An asterisk (*) matches any sequence of characters within a single
//     segment (between colons).
//   - A pattern without wildcards must match the topic exactly.
//
// Examples: "orders:*" matches "orders:list" and "orders:detail" but not
// "orders:detail:item".
func (b *SSEBroker) UseTopics(pattern string, mw Middleware) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topicMiddlewares = append(b.topicMiddlewares, topicMiddleware{
		pattern:    pattern,
		middleware: mw,
	})
}

// applyMiddleware builds the middleware chain for the given topic and returns
// a PublishFunc that, when called, runs the chain and then calls base.
// The chain is: global middleware (outermost first) → matching topic
// middleware (outermost first) → base.
func (b *SSEBroker) applyMiddleware(topic string, base PublishFunc) PublishFunc {
	b.mu.RLock()
	// Fast path: no middleware registered.
	if len(b.middlewares) == 0 && len(b.topicMiddlewares) == 0 {
		b.mu.RUnlock()
		return base
	}
	// Snapshot the slices under read lock.
	globals := make([]Middleware, len(b.middlewares))
	copy(globals, b.middlewares)
	var topicMWs []Middleware
	for _, tm := range b.topicMiddlewares {
		if matchPattern(tm.pattern, topic) {
			topicMWs = append(topicMWs, tm.middleware)
		}
	}
	b.mu.RUnlock()

	// Build the chain inside-out: base ← topic[last] ← … ← topic[0] ← global[last] ← … ← global[0].
	fn := base
	for i := len(topicMWs) - 1; i >= 0; i-- {
		fn = topicMWs[i](fn)
	}
	for i := len(globals) - 1; i >= 0; i-- {
		fn = globals[i](fn)
	}
	return fn
}

// matchPattern reports whether topic matches the wildcard pattern.
// The pattern is split on ":" separators and each segment is compared
// individually.  A "*" segment matches any non-empty segment.  A segment
// without wildcards must match exactly.
func matchPattern(pattern, topic string) bool {
	patParts := strings.Split(pattern, ":")
	topParts := strings.Split(topic, ":")
	if len(patParts) != len(topParts) {
		return false
	}
	for i, pp := range patParts {
		if pp == "*" {
			continue
		}
		if pp != topParts[i] {
			return false
		}
	}
	return true
}
