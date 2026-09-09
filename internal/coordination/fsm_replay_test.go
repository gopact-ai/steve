package coordination

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files this package compares against")

// replayStep is one committed entry and the receipt the machine returned
// for it, as the golden file records them.
type replayStep struct {
	Index   uint64  `json:"index"`
	Command command `json:"command"`
	Receipt receipt `json:"receipt"`
}

type replayGolden struct {
	Steps []replayStep `json:"steps"`
	State State        `json:"state"`
}

// TestApplyReplayIsDeterministic drives every command kind, every rejection
// and the two fatal paths through the state machine in a fixed order and
// compares receipts and final state against a golden file. Raft replays
// the same log on every replica, so the receipts and state a sequence
// produces must never change between builds.
func TestApplyReplayIsDeterministic(t *testing.T) {
	app := openCounter(t, t.TempDir())
	m := newMachine("cluster", app)
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	n1 := Member{NodeID: "n1", Address: "10.0.0.1:1", APIAddress: "10.0.0.1:2", Name: "one", FailureDomain: "rack-1", StorageLevel: "restricted"}
	n2 := Member{NodeID: "n2", Address: "10.0.0.2:1", APIAddress: "10.0.0.2:2", Name: "two", FailureDomain: "rack-2", StorageLevel: "restricted"}
	n3 := Member{NodeID: "n3", Address: "10.0.0.3:1", APIAddress: "10.0.0.3:2", Name: "three", FailureDomain: "rack-3", StorageLevel: "sealed"}
	moved := MemberAddressRequest{ID: "addr-1", NodeID: "n2", Address: "10.0.0.22:1", APIAddress: "10.0.0.22:2"}
	voters := func(index uint64, members ...Member) {
		var servers []raft.Server
		for _, member := range members {
			servers = append(servers, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(member.NodeID), Address: raft.ServerAddress(member.Address)})
		}
		m.StoreConfiguration(index, raft.Configuration{Servers: servers})
	}
	var golden replayGolden
	index := uint64(0)
	rev := func() uint64 { return m.read().Revision }
	apply := func(c command) receipt {
		index++
		c.ClusterID = "cluster"
		c.Time = at
		c.Actor = "test"
		if c.Fingerprint == "" {
			c.Fingerprint = fingerprint(c.Kind, c)
		}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		r, ok := m.Apply(&raft.Log{Index: index, Data: data}).(receipt)
		if !ok {
			t.Fatalf("apply %s: receipt type %T", c.ID, r)
		}
		golden.Steps = append(golden.Steps, replayStep{Index: index, Command: c, Receipt: r})
		return r
	}

	apply(command{Kind: "initialize", ID: "init-before-voters", Member: n1})
	index++
	voters(index, n1)
	apply(command{Kind: "initialize", ID: "init", Member: n1})
	apply(command{Kind: "initialize", ID: "init-again", Member: n1})
	apply(command{Kind: "join_prepare", ID: "prepare-n2-public", Member: Member{NodeID: "n2", Address: n2.Address, StorageLevel: "public"}})
	apply(command{Kind: "join_prepare", ID: "prepare-n2", Member: n2})
	apply(command{Kind: "join_prepare", ID: "prepare-n2-repeat", Member: n2})
	apply(command{Kind: "join_prepare", ID: "prepare-n2-differs", Member: Member{NodeID: "n2", Address: "10.0.0.9:1", StorageLevel: "restricted"}})
	apply(command{Kind: "join_prepare", ID: "prepare-address-taken", Member: Member{NodeID: "n9", Address: n2.Address, FailureDomain: "rack-9", StorageLevel: "restricted"}})
	apply(command{Kind: "join_prepare", ID: "prepare-domain-taken", Member: Member{NodeID: "n9", Address: "10.0.0.9:1", FailureDomain: "rack-2", StorageLevel: "restricted"}})
	apply(command{Kind: "join", ID: "join-n2-early", Member: n2})
	index++
	voters(index, n1, n2)
	apply(command{Kind: "join", ID: "join-n2", Member: n2})
	apply(command{Kind: "join_prepare", ID: "prepare-n3", Member: n3})
	index++
	voters(index, n1, n2, n3)
	apply(command{Kind: "join", ID: "join-n3", Member: n3})
	apply(command{Kind: "policy", ID: "policy-stale", Policy: PolicyRequest{ID: "policy-stale", Enabled: true, ExpectedRevision: 1}})
	apply(command{Kind: "policy", ID: "policy-too-few", Policy: PolicyRequest{ID: "policy-too-few", Enabled: true, ExpectedRevision: rev()}})
	apply(command{Kind: "eligibility", ID: "eligible-stale", Eligibility: EligibilityRequest{ID: "eligible-stale", NodeID: "n2", Eligible: true, ExpectedRevision: 1}})
	apply(command{Kind: "eligibility", ID: "eligible-stranger", Eligibility: EligibilityRequest{ID: "eligible-stranger", NodeID: "n9", Eligible: true, ExpectedRevision: rev()}})
	apply(command{Kind: "eligibility", ID: "eligible-n2", Eligibility: EligibilityRequest{ID: "eligible-n2", NodeID: "n2", Eligible: true, ExpectedRevision: rev()}})
	apply(command{Kind: "eligibility", ID: "eligible-n3", Eligibility: EligibilityRequest{ID: "eligible-n3", NodeID: "n3", Eligible: true, ExpectedRevision: rev()}})
	apply(command{Kind: "policy", ID: "policy-on", Policy: PolicyRequest{ID: "policy-on", Enabled: true, ExpectedRevision: rev()}})
	apply(command{Kind: "eligibility", ID: "eligible-n3-off", Eligibility: EligibilityRequest{ID: "eligible-n3-off", NodeID: "n3", Eligible: false, ExpectedRevision: rev()}})
	apply(command{Kind: "policy", ID: "policy-off", Policy: PolicyRequest{ID: "policy-off", Enabled: false, ExpectedRevision: rev()}})

	apply(command{Kind: "writer", ID: "writer-epoch", Writer: WriterRequest{ID: "writer-epoch", CallerNodeID: "n1", CoordinatorEpoch: 9}})
	apply(command{Kind: "writer", ID: "writer-caller", Writer: WriterRequest{ID: "writer-caller", CallerNodeID: "n2", CoordinatorEpoch: 1}})
	apply(command{Kind: "writer", ID: "writer-generation", Writer: WriterRequest{ID: "writer-generation", CallerNodeID: "n1", CoordinatorEpoch: 1, ExpectedGeneration: 5}})
	apply(command{Kind: "writer", ID: "writer", Writer: WriterRequest{ID: "writer", CallerNodeID: "n1", CoordinatorEpoch: 1}})
	payload := json.RawMessage("3")
	apply(command{Kind: "app", ID: "app-epoch", App: AppCommand{ID: "app-epoch", CallerNodeID: "n1", CoordinatorEpoch: 9, WriterGeneration: 1, Payload: payload}})
	apply(command{Kind: "app", ID: "app-caller", App: AppCommand{ID: "app-caller", CallerNodeID: "n2", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: payload}})
	apply(command{Kind: "app", ID: "app-writer", App: AppCommand{ID: "app-writer", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 0, Payload: payload}})
	apply(command{Kind: "app", ID: "app-version", App: AppCommand{ID: "app-version", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: 4, Payload: payload}})
	apply(command{Kind: "app", ID: "app-1", App: AppCommand{ID: "app-1", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: 0, Payload: payload}})
	apply(command{Kind: "app", ID: "app-2", App: AppCommand{ID: "app-2", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: 1, Payload: payload}})
	apply(command{Kind: "app", ID: "app-1", App: AppCommand{ID: "app-1", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: 0, Payload: payload}})
	apply(command{Kind: "app", ID: "app-1", Fingerprint: "other-input", App: AppCommand{ID: "app-1", CallerNodeID: "n1", CoordinatorEpoch: 1, WriterGeneration: 1, ExpectedVersion: 2, Payload: payload}})

	apply(command{Kind: "address_prepare", ID: "addr-stale", Address: MemberAddressRequest{ID: "addr-stale", NodeID: "n2", Address: moved.Address, ExpectedRevision: 1}})
	apply(command{Kind: "address_prepare", ID: "addr-stranger", Address: MemberAddressRequest{ID: "addr-stranger", NodeID: "n9", Address: moved.Address, ExpectedRevision: rev()}})
	apply(command{Kind: "address_prepare", ID: "addr-taken", Address: MemberAddressRequest{ID: "addr-taken", NodeID: "n2", Address: n3.Address, ExpectedRevision: rev()}})
	moved.ExpectedRevision = rev()
	apply(command{Kind: "address_prepare", ID: "addr-1", Address: moved})
	apply(command{Kind: "address_prepare", ID: "addr-2", Address: MemberAddressRequest{ID: "addr-2", NodeID: "n2", Address: "10.0.0.23:1", ExpectedRevision: rev()}})
	apply(command{Kind: "address", ID: "addr-1-early", Address: moved})
	apply(command{Kind: "address", ID: "addr-1-differs", Address: MemberAddressRequest{ID: "addr-1", NodeID: "n2", Address: "10.0.0.23:1", ExpectedRevision: rev()}})
	index++
	voters(index, n1, Member{NodeID: "n2", Address: moved.Address}, n3)
	configured := index
	apply(command{Kind: "address", ID: "addr-1-commit", Address: moved})

	apply(command{Kind: "transfer", ID: "transfer-epoch", Transfer: TransferRequest{ID: "transfer-epoch", ExpectedEpoch: 9, TargetNodeID: "n2"}, ExpectedAppVersion: 2, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "transfer", ID: "transfer-version", Transfer: TransferRequest{ID: "transfer-version", ExpectedEpoch: 1, TargetNodeID: "n2"}, ExpectedAppVersion: 1, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "transfer", ID: "transfer-stranger", Transfer: TransferRequest{ID: "transfer-stranger", ExpectedEpoch: 1, TargetNodeID: "n9"}, ExpectedAppVersion: 2, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "transfer", ID: "transfer-automatic", Automatic: true, Transfer: TransferRequest{ID: "transfer-automatic", ExpectedEpoch: 1, TargetNodeID: "n2"}, ExpectedAppVersion: 2, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "transfer", ID: "transfer-self", Transfer: TransferRequest{ID: "transfer-self", ExpectedEpoch: 1, TargetNodeID: "n1"}, ExpectedAppVersion: 2, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "transfer", ID: "transfer-n2", Transfer: TransferRequest{ID: "transfer-n2", ExpectedEpoch: 1, TargetNodeID: "n2", Reason: "maintenance"}, ExpectedAppVersion: 2, ExpectedConfigurationIndex: configured})
	apply(command{Kind: "writer", ID: "writer-old-coordinator", Writer: WriterRequest{ID: "writer-old-coordinator", CallerNodeID: "n1", CoordinatorEpoch: 2, ExpectedGeneration: 1}})

	apply(command{Kind: "remove_prepare", ID: "remove-prepare-coordinator", Remove: RemoveRequest{ID: "remove-prepare-coordinator", NodeID: "n2"}})
	apply(command{Kind: "remove_prepare", ID: "remove-prepare-stranger", Remove: RemoveRequest{ID: "remove-prepare-stranger", NodeID: "n9"}})
	apply(command{Kind: "remove_prepare", ID: "remove-prepare-n3", Remove: RemoveRequest{ID: "remove-prepare-n3", NodeID: "n3"}})
	apply(command{Kind: "join_prepare", ID: "prepare-n3-removing", Member: n3})
	apply(command{Kind: "remove", ID: "remove-n3-voter", Remove: RemoveRequest{ID: "remove-n3-voter", NodeID: "n3"}})
	index++
	voters(index, n1, Member{NodeID: "n2", Address: moved.Address})
	apply(command{Kind: "remove", ID: "remove-n3", Remove: RemoveRequest{ID: "remove-n3", NodeID: "n3"}})
	apply(command{Kind: "remove", ID: "remove-coordinator", Remove: RemoveRequest{ID: "remove-coordinator", NodeID: "n2"}})
	apply(command{Kind: "surprise", ID: "unknown"})
	apply(command{Kind: "app", ID: "app-3", App: AppCommand{ID: "app-3", CallerNodeID: "n1", CoordinatorEpoch: 2, WriterGeneration: 1, ExpectedVersion: 2, Payload: payload}})

	// A storage failure in the application stops the replica; everything
	// after it is refused without touching state.
	app.fail = true
	apply(command{Kind: "app", ID: "app-4", App: AppCommand{ID: "app-4", CallerNodeID: "n2", CoordinatorEpoch: 2, WriterGeneration: 1, ExpectedVersion: 2, Payload: payload}})
	apply(command{Kind: "policy", ID: "policy-after-failure", Policy: PolicyRequest{ID: "policy-after-failure", ExpectedRevision: rev()}})
	golden.State = m.read()

	got, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "apply_replay.json")
	if *updateGolden {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), got) {
		t.Fatalf("replay differs from testdata/apply_replay.json; diff the file against this output or regenerate with -update:\n%s", got)
	}
}

// A command from another cluster is fatal on its own, distinct from the
// application failure above.
func TestApplyStopsOnForeignClusterCommand(t *testing.T) {
	m := newMachine("cluster", nil)
	data, err := json.Marshal(command{Kind: "policy", ID: "foreign", ClusterID: "other", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r := m.Apply(&raft.Log{Index: 1, Data: data}).(receipt); r.Code != "application" || m.healthy() {
		t.Fatalf("foreign command receipt %+v, healthy=%v", r, m.healthy())
	}
	if r := m.Apply(&raft.Log{Index: 2, Data: []byte("{not json")}).(receipt); r.Message != "replica is stopped" {
		t.Fatalf("stopped replica answered %+v", r)
	}
	if r := newMachine("cluster", nil).Apply(&raft.Log{Index: 1, Data: []byte("{not json")}).(receipt); r.Code != "application" {
		t.Fatalf("undecodable entry receipt %+v", r)
	}
}

// TestApplyJoinPrepareRejectionDoesNotDependOnMapOrder pins which rejection a
// newcomer gets when it takes one member's address and a different member's
// failure domain. Receipts are replicated state: they are written to the
// snapshot and answer any later entry that reuses the command ID, so replicas
// that disagreed here would answer the same retry differently forever.
func TestApplyJoinPrepareRejectionDoesNotDependOnMapOrder(t *testing.T) {
	n1 := Member{NodeID: "n1", Address: "10.0.0.1:1", FailureDomain: "rack-1", StorageLevel: "restricted"}
	n2 := Member{NodeID: "n2", Address: "10.0.0.2:1", FailureDomain: "rack-2", StorageLevel: "restricted"}
	c := command{Kind: "join_prepare", ID: "prepare-n9", ClusterID: "cluster", Actor: "test", Member: Member{NodeID: "n9", Address: n1.Address, FailureDomain: n2.FailureDomain, StorageLevel: "restricted"}}
	c.Fingerprint = fingerprint(c.Kind, c)
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// Every machine is fresh, so only Go's randomized map iteration can
	// make two of these 200 replays differ.
	for i := range 200 {
		m := newMachine("cluster", nil)
		m.state.Members[n1.NodeID] = n1
		m.state.Members[n2.NodeID] = n2
		r, ok := m.Apply(&raft.Log{Index: 1, Data: data}).(receipt)
		if !ok {
			t.Fatalf("apply returned %T", r)
		}
		if r.Code != "conflict" || r.Message != "address already belongs to another node" {
			t.Fatalf("replay %d rejected with %q / %q", i, r.Code, r.Message)
		}
	}
}
