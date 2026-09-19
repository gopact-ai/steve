package coordination

import (
	"bytes"
	"strconv"
)

// ApplicationReceiptWindow is a replay window in committed application writes,
// not wall-clock time. Administrative receipts and audit records are retained.
const ApplicationReceiptWindow uint64 = 4096

const receiptPruneBatch uint64 = 256

func applicationReplayFloor(version uint64) uint64 {
	if version <= ApplicationReceiptWindow {
		return 0
	}
	return (version - ApplicationReceiptWindow) / receiptPruneBatch * receiptPruneBatch
}

func (m *machine) lookupCommand(c command) (receipt, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c.Kind == "app" && c.App.ExpectedVersion < m.state.AppReplayFloor {
		return expiredReceipt(), true
	}
	r, found := m.receipts[receiptKey(c)]
	if found && r.Fingerprint != c.Fingerprint {
		return receipt{Code: "command", Message: "command ID belongs to different input"}, true
	}
	r.Result.Data = bytes.Clone(r.Result.Data)
	return r, found
}

func receiptKey(c command) string {
	if c.Kind == "app" {
		return "app/" + strconv.FormatUint(c.App.ExpectedVersion, 10) + "/" + c.ID
	}
	return "control/" + c.ID
}

func expiredReceipt() receipt {
	return receipt{Code: "expired", Message: "reconcile committed business facts; do not rebase the original mutation"}
}

// Pruning eligibility follows committed application versions on every replica.
// A local snapshot affects physical SQLite retention, never FSM semantics.
func (m *machine) advanceReplayFloor() {
	next := applicationReplayFloor(m.state.AppVersion)
	if next <= m.state.AppReplayFloor {
		return
	}
	m.state.AppReplayFloor = next
	for id, r := range m.receipts {
		if r.ApplicationVersion != nil && *r.ApplicationVersion < next {
			delete(m.receipts, id)
		}
	}
}
