package readmodel

import (
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
)

// usage reads only closed ledger attempts. Task and plan usage are caches
// of the same executions and must never be added to this accounting.
func usage(closed []attempt.Record, now time.Time) Usage {
	all := newUsageTotals()
	periods := map[string]*usageWindow{
		"1d":  newUsageWindow(now, 1, "hour"),
		"7d":  newUsageWindow(now, 7, "day"),
		"30d": newUsageWindow(now, 30, "day"),
	}
	for _, r := range closed {
		var tokens Tokens
		model := ""
		unreported := 1
		if u := r.Usage; u != nil {
			tokens = Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output, Context: u.Context}
			model = u.Model
			if u.Reported {
				unreported = 0
			}
		}
		var seconds int64
		if !r.StartedAt.IsZero() && !r.EndedAt.IsZero() && r.EndedAt.After(r.StartedAt) {
			seconds = int64(r.EndedAt.Sub(r.StartedAt).Seconds())
		}
		value := UsageRow{Tokens: tokens, Seconds: seconds, Attempts: 1, Unreported: unreported}
		day := ""
		if !r.StartedAt.IsZero() {
			day = r.StartedAt.In(now.Location()).Format("2006-01-02")
		}
		all.add(day, r.Agent, model, value)
		if r.StartedAt.IsZero() || r.StartedAt.After(now) {
			continue
		}
		for _, window := range periods {
			if r.StartedAt.Before(window.from) {
				continue
			}
			// Searching actual bucket starts preserves the two distinct
			// 01:00 hours when the local clock turns back for DST.
			i := sort.Search(len(window.starts), func(i int) bool { return window.starts[i].After(r.StartedAt) }) - 1
			window.totals.add(window.starts[i].Format(time.RFC3339), r.Agent, model, value)
		}
	}
	result := Usage{ByDay: usageRows(all.byTime), ByAgent: usageRows(all.byAgent), ByModel: usageRows(all.byModel), Total: all.total,
		Timezone: now.Location().String(), Periods: make(map[string]UsagePeriod, len(periods))}
	for key, window := range periods {
		series := make([]UsageRow, 0, len(window.starts))
		for _, at := range window.starts {
			key := at.Format(time.RFC3339)
			row := window.totals.byTime[key]
			row.Key = key
			series = append(series, row)
		}
		result.Periods[key] = UsagePeriod{From: window.from, To: now, Interval: window.interval, Series: series,
			ByAgent: usageRows(window.totals.byAgent), ByModel: usageRows(window.totals.byModel), Total: window.totals.total}
	}
	return result
}

type usageWindow struct {
	from     time.Time
	interval string
	starts   []time.Time
	totals   usageTotals
}

func newUsageWindow(now time.Time, days int, interval string) *usageWindow {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	w := &usageWindow{from: today.AddDate(0, 0, 1-days), interval: interval, totals: newUsageTotals()}
	for at := w.from; !at.After(now); {
		w.starts = append(w.starts, at)
		if interval == "hour" {
			at = at.Add(time.Hour)
		} else {
			at = at.AddDate(0, 0, 1)
		}
	}
	return w
}

type usageTotals struct {
	total                    UsageRow
	byTime, byAgent, byModel map[string]UsageRow
}

func newUsageTotals() usageTotals {
	return usageTotals{total: UsageRow{Key: "total"}, byTime: map[string]UsageRow{}, byAgent: map[string]UsageRow{}, byModel: map[string]UsageRow{}}
}

func (u *usageTotals) add(key, agent, model string, value UsageRow) {
	u.total = addUsageRow(u.total, value)
	addUsageGroup(u.byTime, key, value)
	addUsageGroup(u.byAgent, agent, value)
	addUsageGroup(u.byModel, model, value)
}

func addUsageGroup(group map[string]UsageRow, key string, value UsageRow) {
	row := addUsageRow(group[key], value)
	row.Key = key
	group[key] = row
}

func addUsageRow(row, value UsageRow) UsageRow {
	row.Tokens = row.Tokens.add(value.Tokens)
	row.Seconds += value.Seconds
	row.Attempts += value.Attempts
	row.Unreported += value.Unreported
	return row
}

func usageRows(group map[string]UsageRow) []UsageRow {
	rows := make([]UsageRow, 0, len(group))
	for _, row := range group {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}
