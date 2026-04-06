package tavern

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RenderFunc renders content into the provided buffer. It receives the
// context (which is cancelled when the scheduled publisher stops) and a
// shared buffer to write HTML into. Multiple sections write to the same
// buffer in a single tick, so output should be self-contained fragments.
type RenderFunc func(ctx context.Context, buf *bytes.Buffer) error

// ScheduledPublisher manages multiple named sections with independent
// update intervals. It ticks on a fast base interval (default 100ms),
// renders due sections into a shared buffer, and publishes one batched
// message per tick. It automatically skips rendering when no subscribers
// are connected to the topic. ScheduledPublisher is safe for concurrent
// use; sections can be registered while the publisher is running.
type ScheduledPublisher struct {
	broker   *SSEBroker
	event    string
	sections []section
	mu       sync.RWMutex
	baseTick time.Duration
	logger   *slog.Logger
}

// SectionOptions configures optional behavior for a registered section.
// Pass as the last argument to [ScheduledPublisher.Register].
type SectionOptions struct {
	// CircuitBreaker enables circuit breaker protection for the section.
	// When nil, the section renders normally without circuit breaker logic.
	CircuitBreaker *CircuitBreakerConfig
}

type section struct {
	name     string
	interval time.Duration
	render   RenderFunc
	cb       *circuitBreaker // nil when no circuit breaker configured
}

// ScheduledPublisherOption configures the scheduled publisher.
type ScheduledPublisherOption func(*ScheduledPublisher)

// WithBaseTick sets the base tick interval. Default is 100ms.
func WithBaseTick(d time.Duration) ScheduledPublisherOption {
	return func(p *ScheduledPublisher) {
		p.baseTick = d
	}
}

// NewScheduledPublisher creates a publisher that publishes to the given
// event/topic on the broker.
func (b *SSEBroker) NewScheduledPublisher(event string, opts ...ScheduledPublisherOption) *ScheduledPublisher {
	p := &ScheduledPublisher{
		broker:   b,
		event:    event,
		baseTick: 100 * time.Millisecond,
		logger:   b.logger,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Register adds a named section with an interval and render function.
// Sections are rendered in registration order.
func (p *ScheduledPublisher) Register(name string, interval time.Duration, fn RenderFunc, opts ...SectionOptions) {
	s := section{
		name:     name,
		interval: interval,
		render:   fn,
	}
	if len(opts) > 0 && opts[0].CircuitBreaker != nil {
		s.cb = newCircuitBreaker(*opts[0].CircuitBreaker)
	}
	p.mu.Lock()
	p.sections = append(p.sections, s)
	p.mu.Unlock()
}

// SetInterval changes the interval of a registered section at runtime.
// Returns false if the section name is not found.
func (p *ScheduledPublisher) SetInterval(name string, interval time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.sections {
		if p.sections[i].name == name {
			p.sections[i].interval = interval
			return true
		}
	}
	return false
}

// Start begins the publish loop. It blocks until ctx is cancelled.
// Typically called via broker.RunPublisher(ctx, pub.Start) or go pub.Start(ctx).
func (p *ScheduledPublisher) Start(ctx context.Context) {
	ticker := time.NewTicker(p.baseTick)
	defer ticker.Stop()
	buf := new(bytes.Buffer)
	lastSent := make(map[string]time.Time)

	// Seed lastSent so each section waits its full interval before the first render.
	p.mu.RLock()
	now := time.Now()
	for i := range p.sections {
		lastSent[p.sections[i].name] = now
	}
	p.mu.RUnlock()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !p.broker.HasSubscribers(p.event) {
				continue
			}

			buf.Reset()
			p.mu.RLock()
			for i := range p.sections {
				s := &p.sections[i]
				if now.Sub(lastSent[s.name]) < s.interval {
					continue
				}
				p.tickSection(ctx, s, buf, now)
				lastSent[s.name] = now
			}
			p.mu.RUnlock()

			if buf.Len() > 0 {
				p.broker.Publish(p.event, buf.String())
			}
		}
	}
}

// tickSection handles a single section render with circuit breaker and error callback support.
func (p *ScheduledPublisher) tickSection(ctx context.Context, s *section, buf *bytes.Buffer, now time.Time) {
	// No circuit breaker — render directly.
	if s.cb == nil {
		if err := p.renderSection(ctx, s, buf); err != nil {
			p.handleSectionError(s, err, now, 1)
		}
		return
	}

	// Circuit breaker is configured.
	if !s.cb.allow() {
		// Circuit is open — use fallback if available.
		if s.cb.config.FallbackRender != nil {
			buf.WriteString(s.cb.config.FallbackRender())
		}
		return
	}

	if err := p.renderSection(ctx, s, buf); err != nil {
		count := s.cb.recordFailure()
		p.handleSectionError(s, err, now, count)
		// If circuit just opened and fallback exists, render fallback.
		if s.cb.currentState() == circuitOpen && s.cb.config.FallbackRender != nil {
			buf.WriteString(s.cb.config.FallbackRender())
		}
		return
	}

	s.cb.recordSuccess()
}

// handleSectionError logs the error and fires the broker's render error callback.
func (p *ScheduledPublisher) handleSectionError(s *section, err error, now time.Time, count int) {
	if p.logger != nil {
		p.logger.Error("section render error",
			"section", s.name,
			"event", p.event,
			"error", err,
		)
	}
	p.broker.fireRenderError(&RenderError{
		Topic:     p.event,
		Section:   s.name,
		Err:       err,
		Timestamp: now,
		Count:     count,
	})
}

// renderSection calls the render func with panic recovery.
func (p *ScheduledPublisher) renderSection(ctx context.Context, s *section, buf *bytes.Buffer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in section %s: %v", s.name, r)
		}
	}()
	return s.render(ctx, buf)
}
