package tavern

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- circuitBreaker unit tests ---

func TestCircuitBreaker_StartsInClosed(t *testing.T) {
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryInterval: time.Second,
	})
	assert.Equal(t, circuitClosed, cb.currentState())
	assert.True(t, cb.allow())
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryInterval: time.Second,
	})

	cb.recordFailure()
	assert.Equal(t, circuitClosed, cb.currentState())
	assert.True(t, cb.allow())

	cb.recordFailure()
	assert.Equal(t, circuitClosed, cb.currentState())

	cb.recordFailure()
	assert.Equal(t, circuitOpen, cb.currentState())
	assert.False(t, cb.allow())
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryInterval: time.Second,
	})

	cb.recordFailure()
	cb.recordFailure()
	assert.Equal(t, 2, cb.consecutiveFailures())

	cb.recordSuccess()
	assert.Equal(t, 0, cb.consecutiveFailures())
	assert.Equal(t, circuitClosed, cb.currentState())
}

func TestCircuitBreaker_HalfOpenAfterRecoveryInterval(t *testing.T) {
	now := time.Now()
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryInterval: 100 * time.Millisecond,
	})
	cb.nowFunc = func() time.Time { return now }

	cb.recordFailure()
	cb.recordFailure()
	assert.Equal(t, circuitOpen, cb.currentState())
	assert.False(t, cb.allow())

	// Advance time past recovery interval.
	now = now.Add(150 * time.Millisecond)
	assert.True(t, cb.allow())
	assert.Equal(t, circuitHalfOpen, cb.currentState())
}

func TestCircuitBreaker_HalfOpenSuccessCloses(t *testing.T) {
	now := time.Now()
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryInterval: 50 * time.Millisecond,
	})
	cb.nowFunc = func() time.Time { return now }

	cb.recordFailure()
	assert.Equal(t, circuitOpen, cb.currentState())

	now = now.Add(60 * time.Millisecond)
	assert.True(t, cb.allow()) // transitions to half-open
	assert.Equal(t, circuitHalfOpen, cb.currentState())

	cb.recordSuccess()
	assert.Equal(t, circuitClosed, cb.currentState())
	assert.Equal(t, 0, cb.consecutiveFailures())
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	now := time.Now()
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryInterval: 50 * time.Millisecond,
	})
	cb.nowFunc = func() time.Time { return now }

	cb.recordFailure()
	assert.Equal(t, circuitOpen, cb.currentState())

	now = now.Add(60 * time.Millisecond)
	assert.True(t, cb.allow()) // half-open

	cb.recordFailure()
	assert.Equal(t, circuitOpen, cb.currentState())
}

func TestCircuitBreaker_RecordFailureReturnsCount(t *testing.T) {
	cb := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 5,
		RecoveryInterval: time.Second,
	})

	assert.Equal(t, 1, cb.recordFailure())
	assert.Equal(t, 2, cb.recordFailure())
	assert.Equal(t, 3, cb.recordFailure())
}

// --- OnRenderError callback tests ---

func TestOnRenderError_FiredOnScheduledSectionError(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var captured *RenderError
	var mu sync.Mutex
	b.OnRenderError(func(re *RenderError) {
		mu.Lock()
		captured = re
		mu.Unlock()
	})

	pub := b.NewScheduledPublisher("onerr", WithBaseTick(10*time.Millisecond))
	pub.Register("bad-section", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		return errors.New("db connection lost")
	})

	ch, unsub := b.Subscribe("onerr")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	// Wait for at least one tick.
	time.Sleep(60 * time.Millisecond)

	// Drain channel — no messages should arrive since the section errors.
	select {
	case <-ch:
		// might receive empty messages if other sections exist
	default:
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, captured, "OnRenderError callback should have fired")
	assert.Equal(t, "onerr", captured.Topic)
	assert.Equal(t, "bad-section", captured.Section)
	assert.Equal(t, "db connection lost", captured.Err.Error())
	assert.Greater(t, captured.Count, 0)
}

func TestOnRenderError_NotFiredOnSuccess(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var called atomic.Int64
	b.OnRenderError(func(_ *RenderError) {
		called.Add(1)
	})

	pub := b.NewScheduledPublisher("ok", WithBaseTick(10*time.Millisecond))
	pub.Register("good", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("fine")
		return nil
	})

	ch, unsub := b.Subscribe("ok")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for message")
	}

	assert.Equal(t, int64(0), called.Load())
}

func TestOnRenderError_NilCallbackSafe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// No callback registered — should not panic.
	b.fireRenderError(&RenderError{
		Topic: "test",
		Err:   errors.New("oops"),
	})
}

// --- Circuit breaker integration with ScheduledPublisher ---

func TestScheduledPublisher_CircuitBreaker_OpensAfterFailures(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var renderCalls atomic.Int64
	pub := b.NewScheduledPublisher("cb-open", WithBaseTick(10*time.Millisecond))
	pub.Register("flaky", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		renderCalls.Add(1)
		return errors.New("fail")
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 3,
			RecoveryInterval: 5 * time.Second, // long enough that it won't recover during test
		},
	})

	_, unsub := b.Subscribe("cb-open")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	// Wait enough ticks for 3+ failures then circuit opens.
	time.Sleep(150 * time.Millisecond)

	calls := renderCalls.Load()
	// After 3 failures the circuit opens, so calls should stop growing.
	// Wait a bit more and confirm no new calls.
	time.Sleep(100 * time.Millisecond)
	callsAfter := renderCalls.Load()

	// The render should have been called at least 3 times (to trip the breaker)
	// but not much more after that.
	assert.GreaterOrEqual(t, calls, int64(3))
	assert.LessOrEqual(t, callsAfter-calls, int64(1), "render should stop being called after circuit opens")
}

func TestScheduledPublisher_CircuitBreaker_FallbackWhenOpen(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("cb-fallback", WithBaseTick(10*time.Millisecond))
	pub.Register("flaky", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		return errors.New("fail")
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryInterval: 5 * time.Second,
			FallbackRender:   func() string { return "<div>fallback</div>" },
		},
	})

	ch, unsub := b.Subscribe("cb-fallback")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	// First tick: render fails, circuit opens, fallback rendered.
	// Second tick: circuit open, fallback rendered.
	var gotFallback bool
	deadline := time.After(500 * time.Millisecond)
outer:
	for i := 0; i < 5; i++ {
		select {
		case msg := <-ch:
			if msg == "<div>fallback</div>" {
				gotFallback = true
				break outer
			}
		case <-deadline:
			break outer
		}
	}
	assert.True(t, gotFallback, "should receive fallback content when circuit is open")
}

func TestScheduledPublisher_CircuitBreaker_Recovery(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var shouldFail atomic.Bool
	shouldFail.Store(true)

	pub := b.NewScheduledPublisher("cb-recover", WithBaseTick(10*time.Millisecond))
	pub.Register("recovering", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		if shouldFail.Load() {
			return errors.New("fail")
		}
		buf.WriteString("recovered")
		return nil
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 2,
			RecoveryInterval: 80 * time.Millisecond,
		},
	})

	ch, unsub := b.Subscribe("cb-recover")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	// Let it fail and open the circuit.
	time.Sleep(100 * time.Millisecond)

	// Now fix the render function.
	shouldFail.Store(false)

	// Wait for recovery interval + a few ticks.
	var gotRecovered bool
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case msg := <-ch:
			if msg == "recovered" {
				gotRecovered = true
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	assert.True(t, gotRecovered, "should recover after recovery interval when render succeeds")
}

func TestScheduledPublisher_CircuitBreaker_NoFallbackSkipsWhenOpen(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("cb-skip", WithBaseTick(10*time.Millisecond))

	var renderCalls atomic.Int64
	pub.Register("no-fallback", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		renderCalls.Add(1)
		return errors.New("fail")
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryInterval: 5 * time.Second,
			// No FallbackRender
		},
	})

	// Also add a healthy section to confirm messages still flow.
	pub.Register("healthy", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("ok")
		return nil
	})

	ch, unsub := b.Subscribe("cb-skip")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	// Should still receive messages from the healthy section.
	select {
	case msg := <-ch:
		assert.Equal(t, "ok", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for healthy section message")
	}
}

func TestScheduledPublisher_CircuitBreaker_ErrorCallbackWithCount(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var errors_ []*RenderError
	var mu sync.Mutex
	b.OnRenderError(func(re *RenderError) {
		mu.Lock()
		errors_ = append(errors_, re)
		mu.Unlock()
	})

	pub := b.NewScheduledPublisher("cb-count", WithBaseTick(10*time.Millisecond))
	pub.Register("counting", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		return fmt.Errorf("err")
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 3,
			RecoveryInterval: 5 * time.Second,
		},
	})

	_, unsub := b.Subscribe("cb-count")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	time.Sleep(150 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(errors_), 3, "should have at least 3 error callbacks")

	// Verify counts increment.
	assert.Equal(t, 1, errors_[0].Count)
	assert.Equal(t, 2, errors_[1].Count)
	assert.Equal(t, 3, errors_[2].Count)
}

func TestScheduledPublisher_CircuitBreaker_WithRegularSection(t *testing.T) {
	// Ensure that sections without circuit breaker still work fine alongside ones with.
	b := NewSSEBroker()
	defer b.Close()

	pub := b.NewScheduledPublisher("mixed", WithBaseTick(10*time.Millisecond))
	pub.Register("with-cb", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("[cb]")
		return nil
	}, SectionOptions{
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 5,
			RecoveryInterval: time.Second,
		},
	})
	pub.Register("without-cb", 10*time.Millisecond, func(_ context.Context, buf *bytes.Buffer) error {
		buf.WriteString("[plain]")
		return nil
	})

	ch, unsub := b.Subscribe("mixed")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pub.Start(ctx)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "[cb]")
		assert.Contains(t, msg, "[plain]")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out")
	}
}

// --- RenderError struct tests ---

func TestRenderError_ErrorInterface(t *testing.T) {
	underlying := errors.New("something broke")
	re := &RenderError{
		Topic: "test",
		Err:   underlying,
	}
	assert.Equal(t, "something broke", re.Error())
	assert.ErrorIs(t, re, underlying)
}

func TestRenderError_Unwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := fmt.Errorf("wrapped: %w", sentinel)
	re := &RenderError{
		Err: wrapped,
	}
	assert.ErrorIs(t, re, sentinel)
}

// --- RenderComponentErr tests ---

type okComponent struct{}

func (c okComponent) Render(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("<p>ok</p>"))
	return err
}

type errComponent struct{}

func (c errComponent) Render(_ context.Context, _ io.Writer) error {
	return errors.New("component failed")
}

func TestRenderComponentErr_Success(t *testing.T) {
	html, err := RenderComponentErr(okComponent{})
	require.NoError(t, err)
	assert.Equal(t, "<p>ok</p>", html)
}

func TestRenderComponentErr_Error(t *testing.T) {
	html, err := RenderComponentErr(errComponent{})
	require.Error(t, err)
	assert.Equal(t, "", html)
	assert.Equal(t, "component failed", err.Error())
}
