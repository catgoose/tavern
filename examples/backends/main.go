// Command backends runs two brokers over one in-process memory backend and
// prints what each step delivers: fan-out across brokers, scope isolation,
// an unhealthy backend and its recovery, and the backend's counters.
//
// Everything runs in this process. The memory backend stands in for a
// shared bus such as Redis; it shows how a broker uses a backend, not a
// distributed delivery guarantee.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/backend"
	"github.com/catgoose/tavern/backend/memory"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// observed wraps a memory backend and counts what passes through it, to
// show the optional ObservableBackend interface. Health comes from the
// memory backend, which implements HealthAwareBackend.
type observed struct {
	*memory.Backend
	sent, received atomic.Int64
}

var (
	_ backend.ObservableBackend  = (*observed)(nil)
	_ backend.HealthAwareBackend = (*observed)(nil)
)

func (o *observed) Publish(ctx context.Context, env backend.MessageEnvelope) error {
	o.sent.Add(1)
	return o.Backend.Publish(ctx, env)
}

func (o *observed) Subscribe(ctx context.Context, topic string) (<-chan backend.MessageEnvelope, error) {
	in, err := o.Backend.Subscribe(ctx, topic)
	if err != nil {
		return nil, err
	}
	out := make(chan backend.MessageEnvelope, cap(in))
	go func() {
		defer close(out)
		for env := range in {
			o.received.Add(1)
			out <- env
		}
	}()
	return out, nil
}

func (o *observed) Stats() backend.BackendStats {
	return backend.BackendStats{Connected: o.Healthy(), MessagesSent: o.sent.Load(), MessagesReceived: o.received.Load()}
}

const wait = 200 * time.Millisecond

// receive reports the next message on ch, or that none came within wait.
func receive(ch <-chan string) string {
	select {
	case m := <-ch:
		return fmt.Sprintf("%q", m)
	case <-time.After(wait):
		return "nothing"
	}
}

func run(w io.Writer) error {
	bus := memory.New()
	east := &observed{Backend: bus}
	west := &observed{Backend: bus.Fork()}
	a := tavern.NewSSEBroker(tavern.WithBackend(east))
	b := tavern.NewSSEBroker(tavern.WithBackend(west))
	defer func() {
		a.Close()
		b.Close()
		_ = east.Close()
		_ = west.Close()
	}()

	fmt.Fprintln(w, "1. Fan-out: a message published on broker A reaches subscribers on A and B")
	localA, unA := a.Subscribe("news")
	remoteB, unB := b.Subscribe("news")
	time.Sleep(50 * time.Millisecond) // the brokers subscribe to the backend
	a.Publish("news", "hello")
	fmt.Fprintf(w, "   A (local):  %s\n", receive(localA))
	fmt.Fprintf(w, "   B (remote): %s\n", receive(remoteB))

	fmt.Fprintln(w, "2. Scope: a scoped message reaches only that scope on the other broker")
	red, unRed := b.SubscribeScoped("news", "red")
	blue, unBlue := b.SubscribeScoped("news", "blue")
	time.Sleep(50 * time.Millisecond)
	a.PublishTo("news", "red", "for red")
	fmt.Fprintf(w, "   B red:  %s\n", receive(red))
	fmt.Fprintf(w, "   B blue: %s\n", receive(blue))
	fmt.Fprintf(w, "   B unscoped: %s\n", receive(remoteB))

	fmt.Fprintln(w, "3. Unhealthy backend: A stops forwarding, local delivery continues")
	east.SetHealthy(false)
	a.Publish("news", "while down")
	fmt.Fprintf(w, "   A (local):  %s\n", receive(localA))
	fmt.Fprintf(w, "   B (remote): %s\n", receive(remoteB))
	fmt.Fprintf(w, "   A's backend: connected=%t\n", east.Stats().Connected)

	fmt.Fprintln(w, "4. Recovery: healthy again, A resubscribes and forwards new messages")
	east.SetHealthy(true)
	time.Sleep(50 * time.Millisecond)
	a.Publish("news", "back up")
	fmt.Fprintf(w, "   A (local):  %s\n", receive(localA))
	fmt.Fprintf(w, "   B (remote): %s\n", receive(remoteB))
	fmt.Fprintln(w, "   (the message published while down is not redelivered)")

	s := east.Stats()
	r := west.Stats()
	fmt.Fprintln(w, "5. Counters (ObservableBackend.Stats)")
	fmt.Fprintf(w, "   A's backend: sent %d, received %d, connected %t\n", s.MessagesSent, s.MessagesReceived, s.Connected)
	fmt.Fprintf(w, "   B's backend: sent %d, received %d, connected %t\n", r.MessagesSent, r.MessagesReceived, r.Connected)

	unA()
	unB()
	unRed()
	unBlue()
	fmt.Fprintf(w, "6. Cleanup: subscribers left on A %d, on B %d\n", a.SubscriberCount(), b.SubscriberCount())
	return nil
}
