package tavern_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/catgoose/tavern"
)

func ExampleValidateSubscribeOptions() {
	opts := []tavern.SubscribeOption{
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithRate(tavern.Rate{MinInterval: time.Second}),
	}
	err := tavern.ValidateSubscribeOptions(tavern.TargetSubscribeWith, opts...)
	fmt.Println(errors.Is(err, tavern.ErrOptionConflict))

	var oe *tavern.OptionError
	if errors.As(err, &oe) {
		fmt.Println(oe.Option)
	}

	// Options with no effect are reported but accepted by the subscribe call.
	err = tavern.ValidateSubscribeOptions(tavern.TargetSubscribeWith, tavern.SubWithTerminateOnInnerClose())
	fmt.Println(errors.Is(err, tavern.ErrOptionIgnored))
	// Output:
	// true
	// SubWithRate
	// true
}

func ExampleSubWithStatus() {
	broker := tavern.NewSSEBroker(tavern.WithBufferSize(1))
	defer broker.Close()

	status := tavern.NewSubscriptionStatus()
	ch, unsub := broker.SubscribeWith("orders", tavern.SubWithDisconnectOnLoss(), tavern.SubWithStatus(status))
	defer unsub()

	broker.Publish("orders", "changed:1")
	broker.Publish("orders", "changed:2") // the queue is full: the stream ends

	for msg := range ch {
		fmt.Println(msg)
	}
	end := status.Termination()
	fmt.Println(end.Reason, end.Topic)
	// Output:
	// changed:1
	// queue-loss orders
}
