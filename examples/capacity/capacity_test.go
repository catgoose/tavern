package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are small, bounded versions of the presets. They check the
// harness's accounting and teardown so it stays trustworthy; they are not
// performance measurements.

func runScenario(t *testing.T, s Scenario) Report {
	t.Helper()
	s.Timeout = 10 * time.Second
	r, err := Run(s)
	require.NoError(t, err)
	assert.LessOrEqual(t, r.GoroutinesEnd, r.GoroutinesStart+2, "goroutines left behind")
	total := 0
	for _, n := range r.Terminations {
		total += n
	}
	assert.Equal(t, s.Subscribers+s.Slow+s.ReconnectBurst, total, "every subscription ended once")
	return r
}

func TestBrokerFanoutDeliversEverything(t *testing.T) {
	s := Scenario{Name: "fanout", Transport: "broker", Subscribers: 50, Topics: 2, Messages: 20, PayloadBytes: 64, Buffer: 64}
	r := runScenario(t, s)
	assert.Equal(t, int64(50*20), r.FanoutAttempts)
	assert.Zero(t, r.QueueDrops)
	assert.Equal(t, r.FanoutAttempts, r.Received)
	assert.Equal(t, 50, r.Terminations["unsubscribe"])
}

func TestBrokerSlowReadersEndOnLoss(t *testing.T) {
	s := Scenario{Name: "slow", Transport: "broker", Subscribers: 10, Topics: 1, Messages: 40, PayloadBytes: 64,
		Buffer: 32, Slow: 5, LossAware: true}
	r := runScenario(t, s)
	// Non-reading subscribers always overflow; a reader can too if the
	// publisher outruns it, so only lower bounds are certain.
	assert.GreaterOrEqual(t, r.Terminations["queue-loss"], 5, "each non-reading subscriber ends on loss")
	assert.GreaterOrEqual(t, r.QueueDrops, int64(5))
	assert.Equal(t, int64(r.Terminations["queue-loss"]), r.QueueDrops, "a loss-aware subscriber ends at its first drop")
}

func TestBrokerReconnectReplaysOrReportsGap(t *testing.T) {
	full := runScenario(t, Scenario{Name: "replay", Transport: "broker", Subscribers: 20, Topics: 2, Messages: 10,
		PayloadBytes: 64, Buffer: 64, Replay: 10, ReconnectBurst: 20})
	assert.Equal(t, int64(20*9), full.ReconnectReplayed, "everything after ID 1 is replayed")
	assert.Zero(t, full.ReconnectGaps)

	gap := runScenario(t, Scenario{Name: "gap", Transport: "broker", Subscribers: 20, Topics: 2, Messages: 10,
		PayloadBytes: 64, Buffer: 64, Replay: 5, ReconnectBurst: 20})
	assert.Zero(t, gap.ReconnectReplayed)
	assert.Equal(t, int64(20), gap.ReconnectGaps, "ID 1 rolled out of the log")
}

func TestHTTPFanoutAndReconnect(t *testing.T) {
	s := Scenario{Name: "http", Transport: "http", Subscribers: 10, Topics: 2, Messages: 10, PayloadBytes: 64,
		Buffer: 64, Replay: 10, ReconnectBurst: 10}
	r := runScenario(t, s)
	assert.Equal(t, int64(10*10), r.Received, "every frame reached a real client")
	assert.Zero(t, r.QueueDrops)
	assert.Equal(t, int64(10*9), r.ReconnectReplayed)
}

func TestScenarioValidation(t *testing.T) {
	_, err := Run(Scenario{Transport: "carrier-pigeon", Subscribers: 1, Topics: 1, Messages: 1, Buffer: 1, PayloadBytes: 64, Timeout: time.Second})
	assert.Error(t, err)
	for name, p := range presets {
		assert.NoError(t, p.validate(), name)
	}
}

// TestHTTPSetupFailuresCleanUp injects failures at each setup and read step.
// Every run must return an actionable error within its bounds, leave no
// goroutines behind, and not affect the next run.
func TestHTTPSetupFailuresCleanUp(t *testing.T) {
	base := Scenario{Name: "fail", Transport: "http", Subscribers: 4, Topics: 2, Messages: 5, PayloadBytes: 64,
		Buffer: 16, Timeout: 300 * time.Millisecond}
	unavailable := func(match func(*http.Request) bool) func(http.Handler) http.Handler {
		return func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if match(r) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				h.ServeHTTP(w, r)
			})
		}
	}
	cases := []struct {
		name     string
		scenario func(Scenario) Scenario
		inject   func()
		want     string
	}{
		{
			name:     "slow client cannot connect",
			scenario: func(s Scenario) Scenario { s.Slow = 2; return s },
			inject: func() {
				dialSlowClient = func(context.Context, string) (net.Conn, error) { return nil, errors.New("refused") }
			},
			want: "connecting a slow client: refused",
		},
		{
			name:     "slow client dial hangs",
			scenario: func(s Scenario) Scenario { s.Slow = 1; return s },
			inject: func() {
				dialSlowClient = func(ctx context.Context, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
			},
			want: "connecting a slow client: context deadline exceeded",
		},
		{
			name:   "stream refused",
			inject: func() { wrapHandler = unavailable(func(*http.Request) bool { return true }) },
			want:   "503 Service Unavailable",
		},
		{
			name: "no response headers",
			inject: func() {
				wrapHandler = func(http.Handler) http.Handler {
					return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
				}
			},
			// The header timeout and the open deadline are equal; either reports it.
			want: `timeout awaiting response headers|only 0 of 4 clients connected within 300ms`,
		},
		{
			name:     "reconnect refused",
			scenario: func(s Scenario) Scenario { s.Replay = 5; s.ReconnectBurst = 2; return s },
			inject: func() {
				wrapHandler = unavailable(func(r *http.Request) bool { return r.Header.Get("Last-Event-ID") != "" })
			},
			want: "opening reconnecting clients",
		},
		{
			name:     "frame too long to read",
			scenario: func(s Scenario) Scenario { s.PayloadBytes = 4096; return s },
			inject:   func() { maxFrameBytes = 1024 },
			want:     "token too long",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saveDial, saveWrap, saveMax := dialSlowClient, wrapHandler, maxFrameBytes
			t.Cleanup(func() { dialSlowClient, wrapHandler, maxFrameBytes = saveDial, saveWrap, saveMax })
			s := base
			if tc.scenario != nil {
				s = tc.scenario(s)
			}
			tc.inject()

			start := time.Now()
			r, err := Run(s)
			require.Error(t, err)
			assert.Regexp(t, tc.want, err.Error())
			assert.Less(t, time.Since(start), 5*time.Second, "a failed run returns within its bounds")
			assert.LessOrEqual(t, r.GoroutinesEnd, r.GoroutinesStart+2, "a failed run leaves no goroutines behind")

			dialSlowClient, wrapHandler, maxFrameBytes = saveDial, saveWrap, saveMax
			ok, err := Run(Scenario{Name: "after", Transport: "http", Subscribers: 4, Topics: 2, Messages: 5,
				PayloadBytes: 64, Buffer: 16, Slow: 1, Timeout: 10 * time.Second})
			require.NoError(t, err, "the next run is unaffected")
			assert.Equal(t, int64(4*5), ok.Received)
		})
	}
}

func TestScenarioRejectsNegativeAndUnusableInputs(t *testing.T) {
	valid := Scenario{Transport: "broker", Subscribers: 1, Topics: 1, Messages: 1, Buffer: 1, PayloadBytes: 64, Timeout: time.Second}
	for name, mutate := range map[string]func(*Scenario){
		"negative slow":           func(s *Scenario) { s.Slow = -1 },
		"negative replay":         func(s *Scenario) { s.Replay = -1 },
		"negative burst":          func(s *Scenario) { s.ReconnectBurst = -1 },
		"burst without replay":    func(s *Scenario) { s.ReconnectBurst = 1 },
		"burst above subscribers": func(s *Scenario) { s.Replay = 1; s.ReconnectBurst = 2 },
		"payload below timestamp": func(s *Scenario) { s.PayloadBytes = 8 },
		"no timeout":              func(s *Scenario) { s.Timeout = 0 },
	} {
		s := valid
		mutate(&s)
		_, err := Run(s)
		assert.Error(t, err, name)
	}
	_, err := Run(valid)
	assert.NoError(t, err)
}
