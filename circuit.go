package tavern

import (
	"sync"
	"time"
)

// circuitState represents the current state of a circuit breaker.
type circuitState int

const (
	circuitClosed   circuitState = iota // normal operation
	circuitOpen                         // failures exceeded threshold, not calling render
	circuitHalfOpen                     // recovery attempt in progress
)

// CircuitBreakerConfig configures a circuit breaker for a scheduled section.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failures before the
	// circuit opens. Must be at least 1.
	FailureThreshold int

	// RecoveryInterval is how long the circuit stays open before trying a
	// half-open probe. Must be positive.
	RecoveryInterval time.Duration

	// FallbackRender is called when the circuit is open. If nil, the section
	// is skipped while the circuit is open.
	FallbackRender func() string
}

// RenderError contains structured information about a render failure.
type RenderError struct {
	// Topic is the broker topic or scheduled publisher event associated with the error.
	Topic string

	// Section is the section name within a ScheduledPublisher (empty for broker-level errors).
	Section string

	// Err is the underlying error returned by the render function.
	Err error

	// Timestamp is when the error occurred.
	Timestamp time.Time

	// Count is the current consecutive failure count.
	Count int
}

// Error implements the error interface.
func (e *RenderError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *RenderError) Unwrap() error {
	return e.Err
}

// circuitBreaker tracks the state machine for a single section.
type circuitBreaker struct {
	mu           sync.Mutex
	config       CircuitBreakerConfig
	state        circuitState
	failures     int
	lastFailure  time.Time
	nowFunc      func() time.Time // for testing; defaults to time.Now
}

// newCircuitBreaker creates a circuit breaker with the given config.
func newCircuitBreaker(cfg CircuitBreakerConfig) *circuitBreaker {
	return &circuitBreaker{
		config:  cfg,
		state:   circuitClosed,
		nowFunc: time.Now,
	}
}

// allow reports whether the render function should be called. It also
// transitions from open to half-open when the recovery interval has elapsed.
func (cb *circuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case circuitClosed:
		return true
	case circuitOpen:
		if cb.nowFunc().Sub(cb.lastFailure) >= cb.config.RecoveryInterval {
			cb.state = circuitHalfOpen
			return true
		}
		return false
	case circuitHalfOpen:
		// Already probing — allow the single probe call.
		return true
	}
	return false
}

// recordSuccess resets the failure counter and closes the circuit.
func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.state = circuitClosed
}

// recordFailure increments the failure counter and may open the circuit.
// Returns the current consecutive failure count.
func (cb *circuitBreaker) recordFailure() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure = cb.nowFunc()
	if cb.failures >= cb.config.FailureThreshold {
		cb.state = circuitOpen
	}
	return cb.failures
}

// currentState returns the current circuit state (for testing/observability).
func (cb *circuitBreaker) currentState() circuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// consecutiveFailures returns the current failure count (for testing/observability).
func (cb *circuitBreaker) consecutiveFailures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}
