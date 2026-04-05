package taverntest_test

import (
	"testing"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/taverntest"
)

func TestMockBroker_Publish(t *testing.T) {
	m := taverntest.NewMockBroker()

	m.Publish("events", "hello")
	m.Publish("events", "world")
	m.Publish("other", "bye")

	m.AssertPublished(t, "events", "hello")
	m.AssertPublished(t, "events", "world")
	m.AssertPublished(t, "other", "bye")
	m.AssertPublishedCount(t, "events", 2)
	m.AssertPublishedCount(t, "other", 1)
}

func TestMockBroker_PublishTo(t *testing.T) {
	m := taverntest.NewMockBroker()

	m.PublishTo("notif", "user-1", "msg-for-1")

	m.AssertPublished(t, "notif", "msg-for-1")
	m.AssertPublishedCount(t, "notif", 1)

	calls := m.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Scope != "user-1" {
		t.Errorf("expected scope %q, got %q", "user-1", calls[0].Scope)
	}
}

func TestMockBroker_PublishOOB(t *testing.T) {
	m := taverntest.NewMockBroker()

	frag := tavern.Replace("status", "<span>online</span>")
	m.PublishOOB("dashboard", frag)

	rendered := tavern.RenderFragments(frag)
	m.AssertPublished(t, "dashboard", rendered)
	m.AssertPublishedCount(t, "dashboard", 1)

	calls := m.Calls()
	if !calls[0].OOB {
		t.Error("expected OOB flag to be true")
	}
	if len(calls[0].Fragments) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(calls[0].Fragments))
	}
	if calls[0].Fragments[0].ID != "status" {
		t.Errorf("expected fragment ID %q, got %q", "status", calls[0].Fragments[0].ID)
	}
}

func TestMockBroker_PublishOOBTo(t *testing.T) {
	m := taverntest.NewMockBroker()

	frag := tavern.Replace("badge", "<span>3</span>")
	m.PublishOOBTo("notif", "user-1", frag)

	rendered := tavern.RenderFragments(frag)
	m.AssertPublished(t, "notif", rendered)

	calls := m.Calls()
	if calls[0].Scope != "user-1" {
		t.Errorf("expected scope %q, got %q", "user-1", calls[0].Scope)
	}
	if !calls[0].OOB {
		t.Error("expected OOB flag to be true")
	}
}

func TestMockBroker_AssertNotPublished(t *testing.T) {
	m := taverntest.NewMockBroker()

	m.Publish("events", "hello")
	m.AssertNotPublished(t, "other")
}

func TestMockBroker_Reset(t *testing.T) {
	m := taverntest.NewMockBroker()

	m.Publish("events", "hello")
	m.AssertPublishedCount(t, "events", 1)

	m.Reset()
	m.AssertPublishedCount(t, "events", 0)
	m.AssertNotPublished(t, "events")
}

func TestMockBroker_Calls(t *testing.T) {
	m := taverntest.NewMockBroker()

	m.Publish("a", "1")
	m.Publish("b", "2")

	calls := m.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Topic != "a" || calls[0].Msg != "1" {
		t.Errorf("unexpected call[0]: %+v", calls[0])
	}
	if calls[1].Topic != "b" || calls[1].Msg != "2" {
		t.Errorf("unexpected call[1]: %+v", calls[1])
	}

	// Mutating returned slice should not affect mock
	calls[0].Msg = "mutated"
	fresh := m.Calls()
	if fresh[0].Msg != "1" {
		t.Error("Calls() should return a copy")
	}
}
