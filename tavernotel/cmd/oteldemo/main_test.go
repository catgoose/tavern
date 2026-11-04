package main

import (
	"strings"
	"testing"
)

func TestRunCollectsAndUnregisters(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	for _, want := range []string{
		"tavern.messages.published{topic=orders} 6", // deliveries: 3 messages, 2 subscribers
		"tavern.messages.published{topic=other} 1",
		"tavern.subscribers.active 3",
		"collected after unregister: 0 data points",
		"meter provider shut down",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
