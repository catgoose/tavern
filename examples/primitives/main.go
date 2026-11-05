// Command primitives runs the Tavern building blocks that have no page of
// their own in the task board and prints what each one does: scoped topic
// names, option validation, glob subscriptions, multi-topic termination,
// the slow-subscriber callback, and how a replay store keeps (or refuses)
// scoped entries. Everything runs in this process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/catgoose/tavern"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const wait = 200 * time.Millisecond

func next(ch <-chan string) string {
	select {
	case m, ok := <-ch:
		if !ok {
			return "closed"
		}
		return fmt.Sprintf("%q", m)
	case <-time.After(wait):
		return "nothing"
	}
}

func nextTopic(ch <-chan tavern.TopicMessage) string {
	select {
	case m, ok := <-ch:
		if !ok {
			return "closed"
		}
		return fmt.Sprintf("%s %q", m.Topic, m.Data)
	case <-time.After(wait):
		return "nothing"
	}
}

func run(w io.Writer) error {
	scopedTopics(w)
	validation(w)
	globs(w)
	termination(w)
	slowCallback(w)
	return scopedStore(w)
}

// scopedTopics: ScopedTopic only builds a name. Anyone who knows the name
// can subscribe to it; it is not a broker scope and grants nothing.
func scopedTopics(w io.Writer) {
	fmt.Fprintln(w, "1. ScopedTopic builds names; it does not restrict who subscribes")
	b := tavern.NewSSEBroker()
	defer b.Close()
	topic := tavern.ScopedTopic("goals", "team", "12")
	fmt.Fprintf(w, "   name: %s\n", topic)
	fmt.Fprintf(w, "   empty part kept: %q\n", tavern.ScopedTopic("theme", "session", ""))
	anyone, unsub := b.Subscribe(topic)
	defer unsub()
	b.Publish(topic, "goal reached")
	fmt.Fprintf(w, "   a plain subscriber to the name gets: %s\n", next(anyone))
}

// validation: ValidateSubscribeOptions reports every problem without
// subscribing; each is an OptionError naming the target, option and cause.
func validation(w io.Writer) {
	fmt.Fprintln(w, "2. ValidateSubscribeOptions")
	err := tavern.ValidateSubscribeOptions(tavern.TargetSubscribeWith,
		tavern.SubWithRate(tavern.Rate{MinInterval: time.Second}),
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithTerminateOnInnerClose(),
	)
	var errs []error
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	} else if err != nil {
		errs = []error{err}
	}
	for _, e := range errs {
		var oe *tavern.OptionError
		if errors.As(e, &oe) {
			kind := "other"
			switch {
			case errors.Is(oe, tavern.ErrOptionConflict):
				kind = "conflict"
			case errors.Is(oe, tavern.ErrOptionIgnored):
				kind = "ignored"
			case errors.Is(oe, tavern.ErrOptionUnsupported):
				kind = "unsupported"
			case errors.Is(oe, tavern.ErrOptionInvalid):
				kind = "invalid"
			}
			fmt.Fprintf(w, "   %s on %s: %s (%s)\n", oe.Option, oe.Target, kind, oe.Detail)
		}
	}
	fmt.Fprintf(w, "   valid options: %v\n", tavern.ValidateSubscribeOptions(tavern.TargetSubscribeWith, tavern.SubWithScope("red")))
}

// globs: glob patterns match slash-separated topic paths. A scoped glob
// subscriber gets only messages published to its scope.
func globs(w io.Writer) {
	fmt.Fprintln(w, "3. Glob subscriptions")
	b := tavern.NewSSEBroker()
	defer b.Close()
	all, unsubAll := b.SubscribeGlob("orders/**")
	defer unsubAll()
	red, unsubRed := b.SubscribeGlobScoped("orders/**", "red")
	defer unsubRed()
	b.Publish("orders/eu/1", "eu order")
	b.Publish("audit/1", "not an order")
	fmt.Fprintf(w, "   orders/** got: %s, then %s\n", nextTopic(all), nextTopic(all))
	fmt.Fprintf(w, "   scoped red got: %s\n", nextTopic(red))
	b.PublishTo("orders/us/2", "red", "red order")
	fmt.Fprintf(w, "   after PublishTo red: scoped red got %s; orders/** got %s\n", nextTopic(red), nextTopic(all))
	b.UnsubscribeGlob(all)
	fmt.Fprintf(w, "   after UnsubscribeGlob: %s\n", nextTopic(all))
}

// termination: with SubWithTerminateOnInnerClose a multi-topic
// subscription ends when any one topic's subscription ends, and its status
// records the first cause. Without it, the other topics keep delivering.
// Here topic a's subscription is ended with Disconnect.
func termination(w io.Writer) {
	fmt.Fprintln(w, "4. SubWithTerminateOnInnerClose")
	for _, strict := range []bool{true, false} {
		b := tavern.NewSSEBroker()
		status := tavern.NewSubscriptionStatus()
		opts := []tavern.SubscribeOption{tavern.SubWithMeta(tavern.SubscribeMeta{ID: "window"}), tavern.SubWithStatus(status)}
		name := "default"
		if strict {
			opts = append(opts, tavern.SubWithTerminateOnInnerClose())
			name = "terminate on inner close"
		}
		ch, unsub := b.SubscribeMultiWith([]string{"a", "b"}, opts...)
		b.Disconnect("a", "window")
		time.Sleep(50 * time.Millisecond)
		b.Publish("b", "still here")
		got := nextTopic(ch)
		t := status.Termination()
		fmt.Fprintf(w, "   %s: after a is disconnected, b gives %s; status %s", name, got, t.Reason)
		if t.Topic != "" {
			fmt.Fprintf(w, " (topic %q)", t.Topic)
		}
		fmt.Fprintln(w)
		unsub()
		b.Close()
	}
}

// slowCallback: WithSlowSubscriberCallback is told the topic of each
// subscriber evicted for falling behind.
func slowCallback(w io.Writer) {
	fmt.Fprintln(w, "5. WithSlowSubscriberCallback")
	evicted := make(chan string, 4)
	b := tavern.NewSSEBroker(tavern.WithBufferSize(1), tavern.WithSlowSubscriberEviction(2),
		tavern.WithSlowSubscriberCallback(func(topic string) { evicted <- topic }))
	defer b.Close()
	ch, unsub := b.Subscribe("ticker")
	defer unsub()
	for range 4 {
		b.Publish("ticker", "tick") // nobody reads
	}
	select {
	case topic := <-evicted:
		fmt.Fprintf(w, "   callback: subscriber on %q evicted; its channel is %s\n", topic, drainClosed(ch))
	case <-time.After(time.Second):
		fmt.Fprintln(w, "   callback: not called")
	}
}

func drainClosed(ch <-chan string) string {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return "closed"
			}
		case <-time.After(wait):
			return "open"
		}
	}
}

// plainStore is a ReplayStore that does not say it keeps entry scopes.
type plainStore struct{ tavern.ReplayStore }

// scopedStore: a scoped TTL entry is stored with its scope in a store that
// keeps scopes, and replayed only to that scope; a store that does not
// declare it keeps scopes is not given the entry, and the refusal is
// reported.
func scopedStore(w io.Writer) error {
	fmt.Fprintln(w, "6. Scoped entries in a replay store")
	ctx := context.Background()
	store := tavern.NewMemoryReplayStore()
	b := tavern.NewSSEBroker(tavern.WithReplayStore(store))
	defer b.Close()
	b.SetReplayPolicy("notes", 5)
	b.PublishToWithTTL("notes", "red", "for red", time.Minute)
	b.PublishWithTTL("notes", "for everyone", time.Minute)
	entries, err := store.Latest(ctx, "notes", 5)
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Fprintf(w, "   stored %q with scope %q\n", e.Msg, e.Scope)
	}
	blue, unblue := b.SubscribeScoped("notes", "blue")
	defer unblue()
	red, unred := b.SubscribeScoped("notes", "red")
	defer unred()
	fmt.Fprintf(w, "   blue is replayed %s, then %s\n", next(blue), next(blue))
	fmt.Fprintf(w, "   red is replayed %s, then %s\n", next(red), next(red))

	reported := make(chan *tavern.ReplayStoreError, 1)
	plain := plainStore{tavern.NewMemoryReplayStore()}
	b2 := tavern.NewSSEBroker(tavern.WithReplayStore(plain),
		tavern.WithReplayStoreErrorHandler(func(e *tavern.ReplayStoreError) { reported <- e }))
	defer b2.Close()
	b2.PublishToWithTTL("notes", "red", "for red", time.Minute)
	select {
	case e := <-reported:
		fmt.Fprintf(w, "   a store without ScopedReplayStore: %s refused, unsupported=%t\n", e.Op, errors.Is(e, tavern.ErrReplayScopeUnsupported))
	case <-time.After(time.Second):
		fmt.Fprintln(w, "   a store without ScopedReplayStore: no report")
	}
	left, err := plain.Latest(ctx, "notes", 5)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "   entries it holds: %d\n", len(left))
	return nil
}
