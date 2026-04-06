package tavern

import (
	"runtime"
	"strconv"
)

// maxAfterDepth is the maximum nesting depth for After hooks to prevent
// infinite cycles (e.g., After A publishes to B, After B publishes to A).
const maxAfterDepth = 8

// defaultAfterHookConcurrency is the default limit for concurrent After hook
// goroutines. When the semaphore is full, hooks run synchronously.
const defaultAfterHookConcurrency = 64

// afterChain tracks the nesting depth and visited topics within a single
// After hook execution chain.
type afterChain struct {
	depth int
	seen  map[string]struct{}
}

// MutationEvent carries context about a resource mutation. It is passed to
// handlers registered via [SSEBroker.OnMutate] when [SSEBroker.NotifyMutate]
// is called. Resources are logical entities (e.g., "orders") decoupled from
// topic names.
type MutationEvent struct {
	// ID identifies the specific entity that was mutated (e.g., an order ID).
	ID string
	// Data holds the mutated entity or any additional context the handler needs.
	Data any
}

// After registers a callback that fires asynchronously after a successful
// publish to the named topic. Multiple After hooks per topic are allowed and
// execute in registration order. Hooks run in a new goroutine and do not
// block the publish path.
//
// After hooks that publish to other topics may trigger further After hooks.
// To prevent infinite cycles, the broker enforces a maximum nesting depth of 8
// and will skip hooks that would re-enter a topic already in the current chain.
//
// Calling After on a closed broker is a no-op.
func (b *SSEBroker) After(topic string, fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.afterHooks[topic] = append(b.afterHooks[topic], fn)
}

// OnMutate registers a handler for the named resource. The handler fires when
// [SSEBroker.NotifyMutate] is called with the same resource name. Multiple
// handlers per resource are allowed and execute in registration order.
//
// Resources are logical entities (e.g., "orders", "users") rather than topic
// names. This decouples the mutation signal from the specific topics that get
// updated.
//
// Calling OnMutate on a closed broker is a no-op.
func (b *SSEBroker) OnMutate(resource string, fn func(MutationEvent)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.mutateHooks[resource] = append(b.mutateHooks[resource], fn)
}

// NotifyMutate triggers all handlers registered via [SSEBroker.OnMutate] for
// the named resource. Handlers execute synchronously in registration order in
// the caller's goroutine. If no handlers are registered for the resource, this
// is a no-op.
func (b *SSEBroker) NotifyMutate(resource string, event MutationEvent) {
	b.mu.RLock()
	hooks := b.mutateHooks[resource]
	if len(hooks) == 0 {
		b.mu.RUnlock()
		return
	}
	fns := make([]func(MutationEvent), len(hooks))
	copy(fns, hooks)
	b.mu.RUnlock()

	for _, fn := range fns {
		fn(event)
	}
}

// goroutineID returns the current goroutine's numeric identifier extracted
// from runtime.Stack output. This is used solely for After hook chain tracking.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Output starts with "goroutine <id> [..."
	s := buf[:n]
	// Skip "goroutine "
	i := 0
	for i < len(s) && s[i] != ' ' {
		i++
	}
	i++ // skip space
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	id, _ := strconv.ParseUint(string(s[i:j]), 10, 64)
	return id
}

// fireAfterHooks launches After hooks for the given topic in a new goroutine.
// It is called internally after a successful publish. Chain tracking prevents
// infinite cycles when hooks trigger further publishes.
func (b *SSEBroker) fireAfterHooks(topic string) {
	b.mu.RLock()
	hooks := b.afterHooks[topic]
	if len(hooks) == 0 {
		b.mu.RUnlock()
		return
	}
	fns := make([]func(), len(hooks))
	copy(fns, hooks)
	b.mu.RUnlock()

	// Check if we're already inside an After hook chain on this goroutine.
	gid := goroutineID()
	chainVal, nested := b.activeChains.Load(gid)

	var chain *afterChain
	if nested {
		chain = chainVal.(*afterChain)
		if chain.depth >= maxAfterDepth {
			return
		}
		if _, visited := chain.seen[topic]; visited {
			return
		}
		chain.depth++
		chain.seen[topic] = struct{}{}
		for _, fn := range fns {
			fn()
		}
		return
	}

	// Top-level publish — fire hooks asynchronously in a new goroutine if the
	// semaphore has capacity, otherwise run synchronously to avoid dropping hooks.
	run := func() {
		chain = &afterChain{
			depth: 1,
			seen:  map[string]struct{}{topic: {}},
		}
		newGID := goroutineID()
		b.activeChains.Store(newGID, chain)
		defer b.activeChains.Delete(newGID)

		for _, fn := range fns {
			fn()
		}
	}

	select {
	case b.afterHookSem <- struct{}{}:
		// Acquired semaphore slot — run in a new goroutine.
		go func() {
			defer func() { <-b.afterHookSem }()
			run()
		}()
	default:
		// Semaphore full — run synchronously to avoid dropping the hook.
		run()
	}
}

