package tavern

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RenderFunc renders content into the provided buffer.
// It receives the context and a buffer to write into.
type RenderFunc func(ctx context.Context, buf *bytes.Buffer) error

// ScheduledPublisher manages multiple named sections with independent
// update intervals. It ticks on a fast base interval, renders due sections
// into a shared buffer, and publishes one batched message per tick.
type ScheduledPublisher struct {
	broker   *SSEBroker
	event    string
	sections []section
	mu       sync.RWMutex
	baseTick time.Duration
	logger   *slog.Logger
}

type section struct {
	name     string
	interval time.Duration
	render   RenderFunc
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
func (p *ScheduledPublisher) Register(name string, interval time.Duration, fn RenderFunc) {
	p.mu.Lock()
	p.sections = append(p.sections, section{
		name:     name,
		interval: interval,
		render:   fn,
	})
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
				if err := p.renderSection(ctx, s, buf); err != nil {
					if p.logger != nil {
						p.logger.Error("section render error",
							"section", s.name,
							"event", p.event,
							"error", err,
						)
					}
					continue
				}
				lastSent[s.name] = now
			}
			p.mu.RUnlock()

			if buf.Len() > 0 {
				p.broker.Publish(p.event, buf.String())
			}
		}
	}
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
