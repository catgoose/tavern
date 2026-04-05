package taverntest_test

import (
	"testing"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestSimulatedConnection_BasicMessages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	conn := taverntest.NewSimulatedConnection(b, "events")
	defer conn.Close()

	b.Publish("events", "hello")
	b.Publish("events", "world")

	if !conn.WaitFor(2, time.Second) {
		t.Fatal("timed out waiting for messages")
	}

	msgs := conn.Messages()
	if len(msgs) != 2 || msgs[0] != "hello" || msgs[1] != "world" {
		t.Errorf("unexpected messages: %v", msgs)
	}
}

func TestSimulatedConnection_DisconnectReconnect(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	conn := taverntest.NewSimulatedConnection(b, "events")
	defer conn.Close()

	b.Publish("events", "before-disconnect")
	if !conn.WaitFor(1, time.Second) {
		t.Fatal("timed out waiting for first message")
	}

	conn.Disconnect()

	if conn.Connected() {
		t.Error("expected disconnected")
	}

	// Publish while disconnected — should not be received
	b.Publish("events", "while-disconnected")

	conn.Reconnect("")

	if !conn.Connected() {
		t.Error("expected connected after reconnect")
	}

	b.Publish("events", "after-reconnect")

	conn.AssertReceivedAfterReconnect(t, "after-reconnect")

	// Total messages should include before-disconnect and after-reconnect
	msgs := conn.Messages()
	if len(msgs) != 2 {
		t.Errorf("expected 2 total messages, got %d: %v", len(msgs), msgs)
	}
}

func TestSimulatedConnection_ReconnectWithLastEventID(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)

	conn := taverntest.NewSimulatedConnection(b, "events")
	defer conn.Close()

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	if !conn.WaitFor(3, time.Second) {
		t.Fatal("timed out waiting for messages")
	}

	conn.Disconnect()

	// Reconnect from ID "1" — should replay "msg-2" and "msg-3"
	conn.Reconnect("1")

	conn.AssertReceivedAfterReconnect(t, "msg-2", "msg-3")
}

func TestSimulatedConnection_ReconnectMessages(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	conn := taverntest.NewSimulatedConnection(b, "events")
	defer conn.Close()

	b.Publish("events", "first-session")
	if !conn.WaitFor(1, time.Second) {
		t.Fatal("timed out")
	}

	conn.Reconnect("")

	b.Publish("events", "second-session")
	if !conn.WaitForReconnect(1, time.Second) {
		t.Fatal("timed out waiting for reconnect message")
	}

	reconnMsgs := conn.ReconnectMessages()
	if len(reconnMsgs) != 1 || reconnMsgs[0] != "second-session" {
		t.Errorf("unexpected reconnect messages: %v", reconnMsgs)
	}
}

func TestSimulatedConnection_Close_Idempotent(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	conn := taverntest.NewSimulatedConnection(b, "events")
	conn.Close()
	conn.Close() // should not panic
}

func TestSimulatedConnection_Connected(t *testing.T) {
	b := tavern.NewSSEBroker()
	defer b.Close()

	conn := taverntest.NewSimulatedConnection(b, "events")

	if !conn.Connected() {
		t.Error("expected connected after creation")
	}

	conn.Disconnect()
	if conn.Connected() {
		t.Error("expected disconnected")
	}

	conn.Reconnect("")
	if !conn.Connected() {
		t.Error("expected connected after reconnect")
	}

	conn.Close()
}
