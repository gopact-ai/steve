package readmodel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

var (
	ErrHistoryCursor        = errors.New("history: invalid cursor")
	ErrHistoryCursorExpired = errors.New("history: observations changed or expired; reload history")
)

const (
	historyObservation = 0
	historyLedger      = 1
)

// The total order is (UTC time, source, source ID), descending. Ledger IDs
// are sequences; observation IDs are ordinals in the pinned retained prefix.
// Even byte-identical observations remain separate records.
type historyKey struct {
	At     time.Time `json:"at"`
	Source int       `json:"source"`
	ID     int64     `json:"id"`
}

func (a historyKey) less(b historyKey) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	return a.ID < b.ID
}

type historyCursor struct {
	Version      int        `json:"v"`
	Ledger       int64      `json:"ledger"`
	Observations int        `json:"observations"`
	Digest       string     `json:"digest"`
	Before       historyKey `json:"before"`
}

func encodeHistoryCursor(c historyCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeHistoryCursor(value string) (historyCursor, error) {
	var c historyCursor
	if len(value) > 1024 {
		return c, ErrHistoryCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, ErrHistoryCursor
	}
	digest, err := hex.DecodeString(c.Digest)
	if err != nil || len(digest) != sha256.Size || c.Version != 1 || c.Ledger < 0 ||
		c.Observations < 0 || c.Observations > observationsKept || c.Before.ID <= 0 ||
		(c.Before.Source != historyLedger && c.Before.Source != historyObservation) ||
		(c.Before.Source == historyLedger && c.Before.ID > c.Ledger) ||
		(c.Before.Source == historyObservation && c.Before.ID > int64(c.Observations)) ||
		encodeHistoryCursor(c) != value {
		return historyCursor{}, ErrHistoryCursor
	}
	return c, nil
}

func historyObservationDigest(observations []Observation) (string, error) {
	raw, err := json.Marshal(observations)
	if err != nil {
		return "", fmt.Errorf("history: encode observations: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// History returns at most limit entries, newest first across both sources.
// An empty cursor starts a traversal; an empty next means it is exhausted.
// Cursors pin the ledger's sequence fence and the observations' retained
// prefix, so retries and later appends cannot reorder pages. If retention
// removes that prefix, callers must reload instead of silently losing rows.
// Observation persistence and the live event stream are not changed here.
func (m *Model) History(ctx context.Context, cursor string, limit int) ([]HistoryEntry, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	c := historyCursor{Version: 1}
	if cursor != "" {
		var err error
		c, err = decodeHistoryCursor(cursor)
		if err != nil {
			return nil, "", err
		}
	}
	m.mu.Lock()
	retained := m.observations
	if len(retained) > observationsKept {
		retained = retained[len(retained)-observationsKept:]
	}
	observations := make([]Observation, len(retained))
	copy(observations, retained)
	m.mu.Unlock()
	if cursor == "" {
		c.Observations = len(observations)
	} else if len(observations) < c.Observations {
		return nil, "", ErrHistoryCursorExpired
	}
	observations = observations[:c.Observations]
	digest, err := historyObservationDigest(observations)
	if err != nil {
		return nil, "", err
	}
	if cursor != "" && digest != c.Digest {
		return nil, "", ErrHistoryCursorExpired
	}
	c.Digest = digest
	type item struct {
		key   historyKey
		entry HistoryEntry
	}
	items := make([]item, 0, len(observations)+limit+1)
	for i, o := range observations {
		key := historyKey{At: o.At.UTC(), Source: historyObservation, ID: int64(i + 1)}
		if cursor == "" || key.less(c.Before) {
			items = append(items, item{key, HistoryEntry{At: o.At, Kind: "observe." + o.Kind, Subject: o.Subject, Text: o.Text, Data: o.Data}})
		}
	}
	if m.src.Ledger != nil {
		var before *ledger.EventPosition
		var through *int64
		if cursor != "" {
			before = &ledger.EventPosition{At: c.Before.At}
			if c.Before.Source == historyLedger {
				before.Seq = c.Before.ID
			}
			through = &c.Ledger
		}
		events, fence, err := m.src.Ledger.HistoryEvents(ctx, before, through, limit+1)
		if err != nil {
			return nil, "", err
		}
		c.Ledger = fence
		for _, ev := range events {
			items = append(items, item{
				historyKey{At: ev.At.UTC(), Source: historyLedger, ID: ev.Seq},
				HistoryEntry{At: ev.At, Seq: ev.Seq, Kind: "ledger", Subject: ev.OperationID, Operation: ev.OperationID,
					From: ev.From, To: ev.To, Actor: ev.Actor, Text: describeEvent(ev)},
			})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[j].key.less(items[i].key) })
	next := ""
	if len(items) > limit {
		items = items[:limit]
		c.Before = items[len(items)-1].key
		next = encodeHistoryCursor(c)
	}
	out := make([]HistoryEntry, len(items))
	for i, item := range items {
		out[i] = item.entry
	}
	return out, next, nil
}

// HistoryEvents is the chronological journal port; the seq-only Events
// adapter is intentionally not a fallback for history cursors.
func (l Ledger) HistoryEvents(ctx context.Context, before *ledger.EventPosition, through *int64, limit int) ([]ledger.Event, int64, error) {
	if l.Book == nil {
		return nil, 0, errors.New("ledger event source is not configured")
	}
	return l.Book.HistoryEvents(ctx, before, through, limit)
}
