package main

import (
	"encoding/json"
	"testing"
)

func TestAutonomousGateWaitsForEveryParentProcessingReceipt(t *testing.T) {
	for _, pending := range []string{"", "pending", "queued", "uncertain", "suppressed"} {
		var children []task
		if err := json.Unmarshal([]byte(`[{"id":"one","state":"done","result_delivery":{"state":"delivered"}},{"id":"two","state":"done","result_delivery":{"state":"`+pending+`"}}]`), &children); err != nil {
			t.Fatal(err)
		}
		if childrenDelivered(children) {
			t.Fatalf("finished children bypassed %q receipt", pending)
		}
		children[1].ResultDelivery.State = "delivered"
		if !childrenDelivered(children) {
			t.Fatal("settled siblings did not complete")
		}
	}
}
