package main

import (
	"strings"
	"testing"
)

func TestRunShowsEachPrimitive(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	for _, want := range []string{
		"name: goals:team:12",
		`empty part kept: "theme:session:"`,
		`a plain subscriber to the name gets: "goal reached"`,
		"SubWithRate on SubscribeWith: conflict",
		"SubWithTerminateOnInnerClose on SubscribeWith: ignored",
		"valid options: <nil>",
		`orders/** got: orders/eu/1 "eu order", then nothing`,
		"scoped red got: nothing",
		`after PublishTo red: scoped red got orders/us/2 "red order"; orders/** got nothing`,
		"after UnsubscribeGlob: closed",
		`terminate on inner close: after a is disconnected, b gives closed; status disconnect (topic "a")`,
		`default: after a is disconnected, b gives b "still here"; status active`,
		`callback: subscriber on "ticker" evicted; its channel is closed`,
		`stored "for red" with scope "red"`,
		`blue is replayed "for everyone", then nothing`,
		`red is replayed "for red", then "for everyone"`,
		"a store without ScopedReplayStore: append refused, unsupported=true",
		"entries it holds: 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}
