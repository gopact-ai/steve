package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// The replicated table makes effect evidence part of the application state.
// The local append-only file remains a second durable copy for standalone
// database-backup recovery. A replicated reopen never imports that file over
// the consensus application; Raft replay owns recovery in that mode.
func (l *Ledger) seedEffectsJournal() error {
	if l.replicationRequired {
		return l.replaceEffectsJournal()
	}
	entries, err := l.journal.readFileAll()
	if err != nil {
		return err
	}
	tx, err := l.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO effect_entries(seq, data) VALUES (?, ?) ON CONFLICT(seq) DO UPDATE SET data = excluded.data`, entry.Seq, string(raw)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return l.syncEffectsJournal()
}

func (l *Ledger) appendEffect(entry Entry) (Entry, error) {
	tx, err := l.beginWrite(context.Background())
	if err != nil {
		return Entry{}, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(context.Background(), `SELECT COALESCE(MAX(seq), 0) + 1 FROM effect_entries`).Scan(&entry.Seq); err != nil {
		return Entry{}, err
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return Entry{}, err
	}
	if _, err := tx.ExecContext(context.Background(), `INSERT INTO effect_entries(seq, data) VALUES (?, ?)`, entry.Seq, string(raw)); err != nil {
		return Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (l *Ledger) effectEntries(after int64) ([]Entry, error) {
	rows, err := l.db.Query(`SELECT data FROM effect_entries WHERE seq > ? ORDER BY seq`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var entry Entry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (l *Ledger) syncEffectsJournal() error {
	j := l.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := l.effectEntries(j.seq)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err := j.file.Write(append(raw, '\n')); err != nil {
			return fmt.Errorf("effects journal write: %w", err)
		}
		if err := j.file.Sync(); err != nil {
			return fmt.Errorf("effects journal sync: %w", err)
		}
		j.seq = entry.Seq
	}
	return nil
}

func (l *Ledger) replaceEffectsJournal() error {
	j := l.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := l.effectEntries(0)
	if err != nil {
		return err
	}
	var raw []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		raw = append(append(raw, line...), '\n')
	}
	if err := (&FileDocument{Path: j.path}).Save(raw); err != nil {
		return err
	}
	file, err := os.OpenFile(j.path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// The old handle points at the replaced file, whose content Save has
	// already synced; nothing it could report changes the journal.
	_ = j.file.Close()
	j.file, j.seq = file, 0
	if len(entries) > 0 {
		j.seq = entries[len(entries)-1].Seq
	}
	return nil
}
