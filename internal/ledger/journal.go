package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Phase is where an effect is in its life: started before the action
// reaches the world, confirmed once the world has answered.
type Phase string

const (
	PhaseStarted   Phase = "started"
	PhaseConfirmed Phase = "confirmed"
)

// EffectID names one effect instance. The instance key is what makes two
// otherwise identical effects distinct — the path and WAL round of a landing
// write, the occurrence of a message — so a confirmation can only ever be
// paired with the start it belongs to.
type EffectID struct {
	Operation   string `json:"operation"`
	Kind        string `json:"kind"`
	InstanceKey string `json:"instance_key"`
}

func (e EffectID) String() string { return e.Operation + "/" + e.Kind + "/" + e.InstanceKey }

// Entry is one journal line.
type Entry struct {
	Seq         int64           `json:"seq"`
	At          time.Time       `json:"at"`
	Incarnation uint64          `json:"incarnation"`
	Effect      EffectID        `json:"effect"`
	Phase       Phase           `json:"phase"`
	Command     string          `json:"command,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// Journal is the effects journal: append-only, fsync'd per line, and never
// rolled back with the database. What it says started may or may not have
// happened; what it says was confirmed did.
type Journal struct {
	path        string
	incarnation uint64
	now         func() time.Time

	mu   sync.Mutex
	file *os.File
	seq  int64
}

// OpenJournal opens or creates the journal and finds the last sequence.
func OpenJournal(path string, incarnation uint64, now func() time.Time) (*Journal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open effects journal: %w", err)
	}
	j := &Journal{path: path, incarnation: incarnation, now: now, file: file}
	entries, err := j.readAll()
	if err != nil {
		file.Close()
		return nil, err
	}
	if len(entries) > 0 {
		j.seq = entries[len(entries)-1].Seq
	}
	return j, nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.file.Close()
}

// Started records that an effect is about to reach the world. It returns
// only after the line is on disk: the action must not begin before that.
func (j *Journal) Started(effect EffectID, command string, payload any) (Entry, error) {
	return j.append(effect, PhaseStarted, command, payload)
}

// Confirmed records the world's answer to an effect.
func (j *Journal) Confirmed(effect EffectID, receipt any) (Entry, error) {
	return j.append(effect, PhaseConfirmed, "", receipt)
}

func (j *Journal) append(effect EffectID, phase Phase, command string, payload any) (Entry, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Entry{}, err
		}
		raw = b
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	entry := Entry{Seq: j.seq, At: j.now().UTC(), Incarnation: j.incarnation, Effect: effect, Phase: phase, Command: command, Payload: raw}
	line, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, err
	}
	if _, err := j.file.Write(append(line, '\n')); err != nil {
		return Entry{}, fmt.Errorf("effects journal write: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return Entry{}, fmt.Errorf("effects journal sync: %w", err)
	}
	return entry, nil
}

// Outcome is what the journal knows about one effect after reading it all.
type Outcome struct {
	Effect    EffectID
	Started   *Entry
	Confirmed *Entry
}

// Known says whether the effect definitely happened. Only a confirmation
// counts; a start alone is outcome-unknown.
func (o Outcome) Known() bool { return o.Confirmed != nil }

// Reconcile reads the whole journal and pairs starts with confirmations by
// effect id. Recovery walks the result: confirmed effects are folded back
// into the database, started-only effects are marked outcome-unknown, and
// only then does the ledger accept new commands.
func (j *Journal) Reconcile() ([]Outcome, error) {
	entries, err := j.readAll()
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	var out []Outcome
	for i := range entries {
		e := &entries[i]
		key := e.Effect.String()
		pos, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, Outcome{Effect: e.Effect})
			pos = len(out) - 1
		}
		switch e.Phase {
		case PhaseStarted:
			if out[pos].Started == nil {
				out[pos].Started = e
			}
		case PhaseConfirmed:
			out[pos].Confirmed = e
		}
	}
	return out, nil
}

func (j *Journal) readAll() ([]Entry, error) {
	if _, err := j.file.Seek(0, 0); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(j.file)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(raw, []byte{'\n'})
	// A file that ends in '\n' splits into a trailing empty segment. A file
	// that does not was torn mid-write: the writer never returned, so the
	// action that line guarded never started, and the fragment is ignored.
	last := len(lines) - 1
	torn := last >= 0 && len(lines[last]) > 0
	var entries []Entry
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			if torn && i == last {
				break
			}
			return nil, fmt.Errorf("effects journal line %d: %w", i+1, err)
		}
		entries = append(entries, entry)
	}
	if torn {
		// Truncate the fragment so the next line starts clean.
		if err := j.file.Truncate(int64(len(raw) - len(lines[last]))); err != nil {
			return nil, err
		}
	}
	if _, err := j.file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	return entries, nil
}
