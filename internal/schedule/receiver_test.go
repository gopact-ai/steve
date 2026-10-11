package schedule

import (
	"context"
	"testing"
)

type inertReceiver struct{}

func (inertReceiver) ReceiveSchedule(context.Context, Firing) (string, error) { return "accepted", nil }

func TestReceiverRegistryRejectsAmbiguousRegistration(t *testing.T) {
	registry := &ReceiverRegistry{}
	if err := registry.Register("", inertReceiver{}); err == nil {
		t.Fatal("empty channel accepted")
	}
	if err := registry.Register("third", nil); err == nil {
		t.Fatal("nil receiver accepted")
	}
	if err := registry.Register("third", inertReceiver{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("third", inertReceiver{}); err == nil {
		t.Fatal("receiver silently replaced")
	}
}
