package main

import (
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
	assert.Equal(t, s.Subscribers+s.Slow+s.ReconnectBurstSubscriptions(), total, "every subscription ended once")
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
