package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// TransferFacts contains only records explicitly selected by a domain. Lease
// rows, hub credentials and the global command table are never transferred.
type TransferFacts struct {
	Operations []Operation                           `json:"operations,omitempty"`
	Events     []Event                               `json:"events,omitempty"`
	Names      []NamedRef                            `json:"names,omitempty"`
	Bindings   map[string]map[string]json.RawMessage `json:"bindings,omitempty"`
	Effects    []Entry                               `json:"effects,omitempty"`
}

func (f *TransferFacts) Add(other TransferFacts) {
	f.Operations = append(f.Operations, other.Operations...)
	f.Events = append(f.Events, other.Events...)
	f.Names = append(f.Names, other.Names...)
	f.Effects = append(f.Effects, other.Effects...)
	if f.Bindings == nil {
		f.Bindings = map[string]map[string]json.RawMessage{}
	}
	for kind, values := range other.Bindings {
		if f.Bindings[kind] == nil {
			f.Bindings[kind] = map[string]json.RawMessage{}
		}
		for id, raw := range values {
			f.Bindings[kind][id] = raw
		}
	}
}
func (l *Ledger) ExportOperations(ctx context.Context, ids []string) (TransferFacts, error) {
	out := TransferFacts{}
	chosen := map[string]bool{}
	for _, id := range ids {
		if chosen[id] {
			continue
		}
		chosen[id] = true
		op, ok, err := l.Operation(ctx, id)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, fmt.Errorf("operation %s missing", id)
		}
		out.Operations = append(out.Operations, op)
		events, err := l.Events(ctx, id)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, events...)
	}
	l.journal.mu.Lock()
	entries, err := l.journal.readAll()
	l.journal.mu.Unlock()
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if chosen[e.Effect.Operation] {
			out.Effects = append(out.Effects, e)
		}
	}
	return out, nil
}

// ImportFacts validates collisions before writing. Documents are prepared by
// their owning domain and supplied as full merged values, together with exact
// expected previous bytes to make the final multi-domain commit atomic.
func (l *Ledger) ImportFacts(ctx context.Context, f TransferFacts, documents, expected map[string]json.RawMessage) error {
	if err := l.validateImport(ctx, f, documents, expected); err != nil {
		return err
	}
	// Journal identities are stable; importing the same evidence twice does not
	// create a new effect. The atomic database commit follows durable evidence.
	l.journal.mu.Lock()
	old, err := l.journal.readAll()
	l.journal.mu.Unlock()
	if err != nil {
		return err
	}
	seen := map[string]Entry{}
	for _, e := range old {
		seen[e.Effect.String()+"/"+string(e.Phase)] = e
	}
	for _, e := range f.Effects {
		key := e.Effect.String() + "/" + string(e.Phase)
		if was, ok := seen[key]; ok {
			if was.Command != e.Command || !jsonEqual(was.Payload, e.Payload) {
				return fmt.Errorf("effect collision %s", key)
			}
			continue
		}
		if err := l.journal.importEvidence(e); err != nil {
			return err
		}
		seen[key] = e
	}
	return l.Update(ctx, func(tx *Tx) error { return importFactsTx(tx, f, documents, expected, false) })
}
func (l *Ledger) validateImport(ctx context.Context, f TransferFacts, docs, expected map[string]json.RawMessage) error {
	return l.Update(ctx, func(tx *Tx) error { return importFactsTx(tx, f, docs, expected, true) })
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
func importFactsTx(tx *Tx, f TransferFacts, docs, expected map[string]json.RawMessage, validate bool) error {
	existingOps := map[string]Operation{}
	kinds := map[string]bool{}
	for _, op := range f.Operations {
		kinds[op.Kind] = true
	}
	for kind := range kinds {
		rows, err := tx.Operations(kind, "")
		if err != nil {
			return err
		}
		for _, op := range rows {
			existingOps[op.ID] = op
		}
	}
	newOps := map[string]bool{}
	for _, op := range f.Operations {
		if op.ID == "" || op.Kind == "" || !json.Valid(op.Data) {
			return errors.New("invalid operation import")
		}
		var actualKind string
		err := tx.QueryRow("SELECT kind FROM operations WHERE id = ?", op.ID).Scan(&actualKind)
		if err != nil && !errors.Is(err, errNoRows()) {
			return err
		}
		if err == nil {
			old := existingOps[op.ID]
			if actualKind != op.Kind || old.State != op.State || old.Revision != op.Revision || !jsonEqual(old.Data, op.Data) {
				return fmt.Errorf("operation ID collision %s", op.ID)
			}
			continue
		}
		if newOps[op.ID] {
			return fmt.Errorf("duplicate operation %s", op.ID)
		}
		newOps[op.ID] = true
	}
	for _, n := range f.Names {
		var version int64
		var artifact string
		err := tx.QueryRow("SELECT version, artifact FROM names WHERE name = ?", n.Name).Scan(&version, &artifact)
		if err == nil && (version != n.Version || artifact != n.Artifact) {
			return fmt.Errorf("name collision %s", n.Name)
		}
		if err != nil && !errors.Is(err, errNoRows()) {
			return err
		}
	}
	for kind, values := range f.Bindings {
		if kind == documentKind {
			return errors.New("domain documents must use merged document import")
		}
		existing, err := tx.Bindings(kind)
		if err != nil {
			return err
		}
		for id, raw := range values {
			if !json.Valid(raw) {
				return errors.New("invalid binding")
			}
			if old, ok := existing[id]; ok && !jsonEqual(old, raw) {
				return fmt.Errorf("binding collision %s/%s", kind, id)
			}
		}
	}
	for kind, merged := range docs {
		if !json.Valid(merged) {
			return errors.New("invalid merged document")
		}
		raw, found, err := tx.LoadDocument(kind)
		if err != nil {
			return err
		}
		want := expected[kind]
		if found {
			if !jsonEqual(raw, want) && !jsonEqual(raw, merged) {
				return fmt.Errorf("document changed during import: %s", kind)
			}
		} else if len(want) > 0 {
			return fmt.Errorf("document disappeared during import: %s", kind)
		}
	}
	if validate {
		return nil
	}
	for _, op := range f.Operations {
		if !newOps[op.ID] {
			continue
		}
		if _, err := tx.Exec("INSERT INTO operations(id,kind,state,revision,incarnation,data,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", op.ID, op.Kind, op.State, op.Revision, op.Incarnation, string(op.Data), op.CreatedAt.Format(rfc3339nano), op.UpdatedAt.Format(rfc3339nano)); err != nil {
			return err
		}
	}
	for _, e := range f.Events {
		if !newOps[e.OperationID] {
			continue
		}
		fences, _ := json.Marshal(nonNil(e.Fencings))
		effects := e.Effects
		if len(effects) == 0 {
			effects = json.RawMessage("null")
		}
		if _, err := tx.Exec("INSERT INTO events(operation_id,revision,incarnation,from_state,to_state,actor,fencings,effects,at) VALUES(?,?,?,?,?,?,?,?,?)", e.OperationID, e.Revision, e.Incarnation, e.From, e.To, e.Actor, string(fences), string(effects), e.At.Format(rfc3339nano)); err != nil {
			return err
		}
	}
	for _, n := range f.Names {
		if _, err := tx.Exec("INSERT OR IGNORE INTO names(name,version,artifact,updated_at) VALUES(?,?,?,?)", n.Name, n.Version, n.Artifact, n.UpdatedAt.Format(rfc3339nano)); err != nil {
			return err
		}
	}
	for kind, values := range f.Bindings {
		for id, raw := range values {
			if err := tx.PutBinding(kind, id, json.RawMessage(raw)); err != nil {
				return err
			}
		}
	}
	for kind, raw := range docs {
		if err := tx.PutBinding(documentKind, kind, json.RawMessage(raw)); err != nil {
			return err
		}
	}
	return nil
}

// StagedDocument is an in-memory domain document used to prepare an offline
// import without exposing partially imported state to the destination hub.
type StagedDocument struct {
	Raw    []byte
	Exists bool
}

func (d *StagedDocument) Load() ([]byte, bool, error) {
	return append([]byte(nil), d.Raw...), d.Exists, nil
}
func (d *StagedDocument) Save(raw []byte) error {
	d.Raw = append([]byte(nil), raw...)
	d.Exists = true
	return nil
}
func (d *StagedDocument) Check() error { return nil }

// ValidateImport performs collision checks without changing durable facts.
func (l *Ledger) ValidateImport(ctx context.Context, f TransferFacts, docs, expected map[string]json.RawMessage) error {
	return l.validateImport(ctx, f, docs, expected)
}

// StoreDocument writes a domain-prepared document in the owning transaction.
func (t *Tx) StoreDocument(kind string, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid domain document")
	}
	return t.PutBinding(documentKind, kind, raw)
}

// RecordTransition applies an already-validated maintenance transition in a
// larger domain transaction and retains its event history.
func (t *Tx) RecordTransition(op Operation, to, actor string) error {
	now := t.l.now().UTC()
	revision := op.Revision + 1
	res, err := t.Exec("UPDATE operations SET state=?,revision=?,data=?,updated_at=? WHERE id=? AND revision=?", to, revision, string(op.Data), now.Format(rfc3339nano), op.ID, op.Revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("operation changed during maintenance: %s", op.ID)
	}
	_, err = t.Exec("INSERT INTO events(operation_id,revision,incarnation,from_state,to_state,actor,fencings,effects,at) VALUES(?,?,?,?,?,?,?,?,?)", op.ID, revision, t.l.incarnation, op.State, to, actor, "[]", "null", now.Format(rfc3339nano))
	return err
}

// importEvidence keeps original timestamps/incarnations as provenance while
// assigning a monotonic destination journal sequence. It dispatches nothing.
func (j *Journal) importEvidence(entry Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if entry.Phase != PhaseStarted && entry.Phase != PhaseConfirmed {
		return errors.New("invalid imported effect phase")
	}
	entry.Seq = j.seq + 1
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := j.file.Write(raw); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.seq = entry.Seq
	return nil
}
