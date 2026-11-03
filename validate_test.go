package tavern

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// problem is an expected (option, kind) pair from ValidateSubscribeOptions.
type problem struct {
	option string
	kind   error
}

func problemsOf(t *testing.T, err error) []problem {
	t.Helper()
	if err == nil {
		return nil
	}
	var joined interface{ Unwrap() []error }
	var list []error
	if errors.As(err, &joined) {
		list = joined.Unwrap()
	} else {
		list = []error{err}
	}
	got := make([]problem, 0, len(list))
	for _, e := range list {
		var oe *OptionError
		require.ErrorAs(t, e, &oe)
		got = append(got, problem{oe.Option, oe.Err})
	}
	sort.Slice(got, func(i, j int) bool { return got[i].option < got[j].option })
	return got
}

// subscribeTo calls the real subscribe method for target and unsubscribes.
func subscribeTo(b *SSEBroker, target SubscribeTarget, opts []SubscribeOption) {
	switch target {
	case TargetSubscribeWith:
		_, unsub := b.SubscribeWith("t", opts...)
		unsub()
	case TargetSubscribeFromIDWith:
		_, unsub := b.SubscribeFromIDWith("t", "", opts...)
		unsub()
	case TargetSubscribeMultiWith:
		_, unsub := b.SubscribeMultiWith([]string{"a", "b"}, opts...)
		unsub()
	case TargetSubscribeGlobWith:
		_, unsub := b.SubscribeGlobWith("a/*", opts...)
		unsub()
	}
}

var allTargets = []SubscribeTarget{
	TargetSubscribeWith, TargetSubscribeFromIDWith, TargetSubscribeMultiWith, TargetSubscribeGlobWith,
}

// TestValidateSubscribeOptionsMatchesExecution checks each case's reported
// problems and that the real subscribe method panics exactly when a problem
// other than ErrOptionIgnored is reported.
func TestValidateSubscribeOptionsMatchesExecution(t *testing.T) {
	rate := SubWithRate(Rate{MinInterval: time.Second})
	cases := []struct {
		name string
		opts func() []SubscribeOption
		want map[SubscribeTarget][]problem
	}{
		{
			name: "all supported single-topic options",
			opts: func() []SubscribeOption {
				return []SubscribeOption{SubWithScope("s"), SubWithFilter(acceptAll), rate,
					SubWithMeta(SubscribeMeta{ID: "u"}), SubWithSnapshot(snapshotValue), SubWithStatus(NewSubscriptionStatus())}
			},
			want: map[SubscribeTarget][]problem{
				TargetSubscribeGlobWith: {
					{"SubWithMeta", ErrOptionIgnored}, {"SubWithSnapshot", ErrOptionIgnored},
					{"SubWithStatus", ErrOptionUnsupported},
				},
			},
		},
		{
			name: "loss with rate",
			opts: func() []SubscribeOption { return []SubscribeOption{SubWithDisconnectOnLoss(), rate} },
			want: map[SubscribeTarget][]problem{
				TargetSubscribeWith:       {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeFromIDWith: {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeMultiWith:  {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeGlobWith:   {{"SubWithDisconnectOnLoss", ErrOptionUnsupported}},
			},
		},
		{
			name: "loss with zero rate still conflicts",
			opts: func() []SubscribeOption { return []SubscribeOption{SubWithDisconnectOnLoss(), SubWithRate(Rate{})} },
			want: map[SubscribeTarget][]problem{
				TargetSubscribeWith:       {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeFromIDWith: {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeMultiWith:  {{"SubWithRate", ErrOptionConflict}},
				TargetSubscribeGlobWith:   {{"SubWithDisconnectOnLoss", ErrOptionUnsupported}, {"SubWithRate", ErrOptionIgnored}},
			},
		},
		{
			name: "terminate on inner close",
			opts: func() []SubscribeOption { return []SubscribeOption{SubWithTerminateOnInnerClose()} },
			want: map[SubscribeTarget][]problem{
				TargetSubscribeWith:       {{"SubWithTerminateOnInnerClose", ErrOptionIgnored}},
				TargetSubscribeFromIDWith: {{"SubWithTerminateOnInnerClose", ErrOptionIgnored}},
				TargetSubscribeGlobWith:   {{"SubWithTerminateOnInnerClose", ErrOptionIgnored}},
			},
		},
		{
			name: "replay error termination with a filter",
			opts: func() []SubscribeOption {
				return []SubscribeOption{SubWithTerminateOnReplayError(), SubWithFilter(acceptAll)}
			},
			want: map[SubscribeTarget][]problem{
				TargetSubscribeWith:      {{"SubWithTerminateOnReplayError", ErrOptionIgnored}},
				TargetSubscribeMultiWith: {{"SubWithTerminateOnReplayError", ErrOptionIgnored}},
				TargetSubscribeGlobWith:  {{"SubWithTerminateOnReplayError", ErrOptionUnsupported}},
			},
		},
		{
			name: "replay error termination with loss and a snapshot",
			opts: func() []SubscribeOption {
				return []SubscribeOption{SubWithTerminateOnReplayError(), SubWithDisconnectOnLoss(), SubWithSnapshot(snapshotValue)}
			},
			want: map[SubscribeTarget][]problem{
				TargetSubscribeGlobWith: {
					{"SubWithDisconnectOnLoss", ErrOptionUnsupported}, {"SubWithSnapshot", ErrOptionIgnored},
					{"SubWithTerminateOnReplayError", ErrOptionUnsupported},
				},
			},
		},
		{
			name: "no-op values",
			opts: func() []SubscribeOption {
				return []SubscribeOption{SubWithScope(""), SubWithFilter(nil), SubWithSnapshot(nil), SubWithStatus(nil), SubWithRate(Rate{})}
			},
			want: func() map[SubscribeTarget][]problem {
				noop := []problem{
					{"SubWithFilter", ErrOptionIgnored}, {"SubWithRate", ErrOptionIgnored},
					{"SubWithScope", ErrOptionIgnored}, {"SubWithSnapshot", ErrOptionIgnored},
					{"SubWithStatus", ErrOptionIgnored},
				}
				return map[SubscribeTarget][]problem{
					TargetSubscribeWith: noop, TargetSubscribeFromIDWith: noop, TargetSubscribeMultiWith: noop,
					TargetSubscribeGlobWith: noop,
				}
			}(),
		},
		{
			name: "nil option",
			opts: func() []SubscribeOption { return []SubscribeOption{SubWithScope("s"), nil} },
			want: map[SubscribeTarget][]problem{
				TargetSubscribeWith:       {{"option 1", ErrOptionInvalid}},
				TargetSubscribeFromIDWith: {{"option 1", ErrOptionInvalid}},
				TargetSubscribeMultiWith:  {{"option 1", ErrOptionInvalid}},
				TargetSubscribeGlobWith:   {{"option 1", ErrOptionInvalid}},
			},
		},
	}
	for _, tc := range cases {
		for _, target := range allTargets {
			t.Run(tc.name+"/"+target.String(), func(t *testing.T) {
				want := tc.want[target]
				got := problemsOf(t, ValidateSubscribeOptions(target, tc.opts()...))
				sort.Slice(want, func(i, j int) bool { return want[i].option < want[j].option })
				assert.Equal(t, want, got)

				panics := false
				for _, p := range got {
					if p.kind != ErrOptionIgnored {
						panics = true
					}
				}
				b := NewSSEBroker()
				defer b.Close()
				run := func() { subscribeTo(b, target, tc.opts()) }
				if panics {
					assert.Panics(t, run)
				} else {
					assert.NotPanics(t, run)
				}
			})
		}
	}
}

func TestValidateSubscribeOptionsHasNoSideEffects(t *testing.T) {
	var snapshots, filters atomic.Int64
	status := NewSubscriptionStatus()
	opts := []SubscribeOption{
		SubWithSnapshot(func() string { snapshots.Add(1); return "snap" }),
		SubWithFilter(func(string) bool { filters.Add(1); return true }),
		SubWithStatus(status),
	}
	for range 3 {
		assert.NoError(t, ValidateSubscribeOptions(TargetSubscribeWith, opts...))
	}
	invalid := append([]SubscribeOption{SubWithDisconnectOnLoss(), SubWithRate(Rate{MinInterval: time.Second})}, opts...)
	assert.ErrorIs(t, ValidateSubscribeOptions(TargetSubscribeMultiWith, invalid...), ErrOptionConflict)
	assert.ErrorIs(t, ValidateSubscribeOptions(TargetSubscribeGlobWith, opts...), ErrOptionUnsupported)

	assert.Zero(t, snapshots.Load())
	assert.Zero(t, filters.Load())
	assert.Equal(t, TerminationNone, status.Termination().Reason)
	select {
	case <-status.Done():
		t.Fatal("validation ended the status")
	default:
	}

	// The validated status still attaches to one real subscription.
	b := NewSSEBroker()
	defer b.Close()
	_, unsub := b.SubscribeWith("t", opts...)
	assert.Equal(t, int64(1), snapshots.Load())
	unsub()
	assert.Equal(t, TerminationUnsubscribe, waitTermination(t, status).Reason)

	err := ValidateSubscribeOptions(TargetSubscribeWith, SubWithStatus(status))
	assert.ErrorIs(t, err, ErrOptionInvalid)
	assert.Panics(t, func() { b.SubscribeWith("t", SubWithStatus(status)) })
}

func TestOptionErrorDetails(t *testing.T) {
	err := ValidateSubscribeOptions(TargetSubscribeGlobWith, SubWithStatus(NewSubscriptionStatus()))
	var oe *OptionError
	require.ErrorAs(t, err, &oe)
	assert.Equal(t, TargetSubscribeGlobWith, oe.Target)
	assert.Equal(t, "SubWithStatus", oe.Option)
	assert.ErrorIs(t, oe, ErrOptionUnsupported)
	assert.Equal(t, "tavern: SubscribeGlobWith: SubWithStatus: unsupported option: glob subscriptions do not support it", oe.Error())

	err = ValidateSubscribeOptions(SubscribeTarget(0))
	assert.ErrorIs(t, err, ErrOptionInvalid)
	assert.NoError(t, ValidateSubscribeOptions(TargetSubscribeMultiWith))
}

// TestLegacyOptionPanicsUnchanged pins the panic messages the subscribe
// methods already used, and that a status survives a rejected call.
func TestLegacyOptionPanicsUnchanged(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	rate := SubWithRate(Rate{MinInterval: time.Second})
	assert.PanicsWithValue(t, "tavern: SubWithDisconnectOnLoss cannot be combined with SubWithRate", func() {
		b.SubscribeWith("t", SubWithDisconnectOnLoss(), rate)
	})
	for opt, msg := range map[string]SubscribeOption{
		"tavern: SubscribeGlobWith does not support SubWithDisconnectOnLoss":       SubWithDisconnectOnLoss(),
		"tavern: SubscribeGlobWith does not support SubWithStatus":                 SubWithStatus(NewSubscriptionStatus()),
		"tavern: SubscribeGlobWith does not support SubWithTerminateOnReplayError": SubWithTerminateOnReplayError(),
	} {
		assert.PanicsWithValue(t, opt, func() { b.SubscribeGlobWith("a/*", msg) })
	}

	status := NewSubscriptionStatus()
	assert.Panics(t, func() { b.SubscribeWith("t", SubWithDisconnectOnLoss(), rate, SubWithStatus(status)) })
	_, unsub := b.SubscribeWith("t", SubWithStatus(status))
	unsub()
	assert.PanicsWithValue(t, "tavern: SubscriptionStatus is already attached to a subscription", func() {
		b.SubscribeWith("t", SubWithStatus(status))
	})
}

// TestStatusAttachRace validates and subscribes with one status from many
// goroutines. Exactly one subscription attaches it.
func TestStatusAttachRace(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	status := NewSubscriptionStatus()
	var attached, rejected atomic.Int64
	var unsubs sync.Map
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = ValidateSubscribeOptions(TargetSubscribeWith, SubWithStatus(status))
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					rejected.Add(1)
				}
			}()
			_, unsub := b.SubscribeWith("t", SubWithStatus(status))
			attached.Add(1)
			unsubs.Store(i, unsub)
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1), attached.Load())
	assert.Equal(t, int64(15), rejected.Load())
	unsubs.Range(func(_, v any) bool { v.(func())(); return true })
}

// drainNow returns everything already queued on ch.
func drainNow(ch <-chan string) []string {
	var got []string
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, msg)
		default:
			return got
		}
	}
}

// TestSubscriptionGuideReplayDifferences pins the fresh-subscribe replay
// differences the subscription guide documents.
func TestSubscriptionGuideReplayDifferences(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	b.SetReplayPolicy("t", 5)
	b.PublishWithID("t", "1", "a")
	b.PublishWithID("t", "2", "b")
	snap := SubWithSnapshot(func() string { return "snap" })
	onlyB := SubWithFilter(func(m string) bool { return m == "b" || m == "snap" })

	cases := []struct {
		name      string
		subscribe func() (<-chan string, func())
		want      []string
	}{
		{"SubscribeWith replays plain messages", func() (<-chan string, func()) { return b.SubscribeWith("t") },
			[]string{"a", "b"}},
		{"snapshot without filter has no replay", func() (<-chan string, func()) { return b.SubscribeWith("t", snap) },
			[]string{"snap"}},
		{"snapshot with filter replays the filtered cache", func() (<-chan string, func()) { return b.SubscribeWith("t", snap, onlyB) },
			[]string{"snap", "b"}},
		{"SubscribeFromIDWith replays with ids after the snapshot", func() (<-chan string, func()) {
			return b.SubscribeFromIDWith("t", "", snap)
		}, []string{"snap", "a\nid: 1", "b\nid: 2"}},
		{"loss-aware SubscribeWith replays like SubscribeFromIDWith", func() (<-chan string, func()) {
			return b.SubscribeWith("t", SubWithDisconnectOnLoss())
		}, []string{"a\nid: 1", "b\nid: 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, unsub := tc.subscribe()
			defer unsub()
			assert.Equal(t, tc.want, drainNow(ch))
		})
	}
}
