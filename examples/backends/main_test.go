package main

import (
	"strings"
	"testing"
)

func TestRunShowsEachStep(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	for _, want := range []string{
		`A (local):  "hello"`,
		`B (remote): "hello"`,
		`B red:  "for red"`,
		"B blue: nothing",
		"B unscoped: nothing",
		`A (local):  "while down"`,
		"B (remote): nothing",
		"A's backend: connected=false",
		`B (remote): "back up"`,
		"A's backend: sent 3, received 0, connected true", // hello, for red, back up
		"B's backend: sent 0, received 3, connected true",
		"subscribers left on A 0, on B 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
