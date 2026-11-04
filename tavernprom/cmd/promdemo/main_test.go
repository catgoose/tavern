package main

import (
	"strings"
	"testing"
)

func TestRunScrapesAndUnregisters(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	for _, want := range []string{
		`demo_messages_published_total{topic="orders"} 10`,
		`demo_messages_published_total{topic="other"} 2`,
		`demo_subscribers_active 3`,
		"scrape after unregister: 0 demo_ series",
		`own_messages_published_total{topic="orders"} 10`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
