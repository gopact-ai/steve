package readmodel

import (
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
)

// usage counts only the ledger's settled attempts. Tasks supply ownership and
// trigger metadata; their cached token totals never enter this accounting.
func usage(closed []attempt.UsageSample, now time.Time, tasks []Task) Usage {
	roots := taskRoots(tasks)
	all := newUsageTotals(time.Time{}, now)
	periods := map[string]*usageWindow{
		"1d": newUsageWindow(now, 1, "hour"), "7d": newUsageWindow(now, 7, "day"), "30d": newUsageWindow(now, 30, "day"),
	}
	samples := make([]usageSample, len(closed))
	scratch := usageScratch{
		spans: make([]usageSpan, 0, len(closed)), events: make([]usageEvent, 0, 2*len(closed)), points: make([]curvePoint, 0, 2*len(closed)),
		tasks: make(map[string]taskWork),
	}
	for i, r := range closed {
		sample := &samples[i]
		sample.root = roots[r.TaskID]
		sample.agent, sample.harness, sample.project = r.Agent, r.Harness, r.Project
		if r.TaskID != "" && sample.root.id == "" {
			sample.root.id = r.TaskID
		}
		if sample.project == "" {
			sample.project = sample.root.project
		}
		sample.row = UsageRow{Attempts: 1, Unreported: 1}
		if u := r.Usage; u != nil {
			sample.model = u.Model
			sample.row.Tokens.Context = u.Context
			if u.Reported {
				sample.row.Unreported = 0
				sample.row.Tokens = Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output, Context: u.Context}
			}
		}
		if !r.StartedAt.IsZero() && !r.EndedAt.IsZero() && r.EndedAt.After(r.StartedAt) {
			sample.row.Seconds = int64(r.EndedAt.Sub(r.StartedAt).Seconds())
		}
		// Windows include attempts by start time, so their lower bound never
		// clips an included interval. The upper bound is the same snapshot
		// time everywhere; share that measurement across all groups.
		sample.measurement = measureUsage(r.StartedAt, r.EndedAt, now, sample.row.Tokens.Total)
		day := ""
		if !r.StartedAt.IsZero() {
			day = r.StartedAt.In(now.Location()).Format("2006-01-02")
		}
		all.add(day, sample)
		if r.StartedAt.IsZero() || r.StartedAt.After(now) {
			continue
		}
		for _, window := range periods {
			if r.StartedAt.Before(window.from) {
				continue
			}
			i := sort.Search(len(window.starts), func(i int) bool { return window.starts[i].After(r.StartedAt) }) - 1
			window.totals.add(window.keys[i], sample)
		}
	}
	stats, byTask := all.total.taskSummary(&scratch)
	throughput := all.total.throughput(all.from, now, all.total.curve(&scratch), &scratch)
	result := Usage{ByDay: all.rows(all.byTime, &scratch), ByAgent: all.rows(all.byAgent, &scratch), ByModel: all.rows(all.byModel, &scratch), Total: all.total.row,
		ByHarness: all.rows(all.byHarness, &scratch), ByTrigger: all.rows(all.byTrigger, &scratch), ByProject: all.rows(all.byProject, &scratch), Tasks: stats, ByTask: byTask,
		Throughput: throughput, Timezone: now.Location().String(), Periods: make(map[string]UsagePeriod, len(periods))}
	result.Total.Key = "total"
	for key, window := range periods {
		u := window.totals
		series := make([]UsageRow, 0, len(window.starts))
		curve := u.total.curve(&scratch)
		for i, at := range window.starts {
			key := window.keys[i]
			row := UsageRow{Key: key}
			if group := u.byTime[key]; group != nil {
				row = group.row
				row.Key = key
				stats, _ := group.taskSummary(&scratch)
				row.Tasks = &stats
			}
			end := now
			if i+1 < len(window.starts) {
				end = window.starts[i+1]
			}
			if end.After(at) {
				row.TPM = (curve.at(end) - curve.at(at)) / end.Sub(at).Minutes()
			}
			series = append(series, row)
		}
		stats, byTask := u.total.taskSummary(&scratch)
		throughput := u.total.throughput(window.from, now, curve, &scratch)
		total := u.total.row
		total.Key = "total"
		result.Periods[key] = UsagePeriod{From: window.from, To: now, Interval: window.interval, Series: series,
			ByAgent: u.rows(u.byAgent, &scratch), ByModel: u.rows(u.byModel, &scratch), Total: total,
			ByHarness: u.rows(u.byHarness, &scratch), ByTrigger: u.rows(u.byTrigger, &scratch), ByProject: u.rows(u.byProject, &scratch), Tasks: stats, ByTask: byTask,
			Throughput: throughput}
	}
	return result
}

type rootTask struct{ id, title, trigger, project string }

func taskRoots(tasks []Task) map[string]rootTask {
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	out := make(map[string]rootTask, len(tasks))
	for _, item := range tasks {
		root := rootTask{id: item.ID}
		seen := map[string]bool{}
		for id := item.ID; id != "" && !seen[id]; {
			seen[id] = true
			t, exists := byID[id]
			if !exists {
				break
			}
			if t.Parent == "" {
				trigger := t.Origin
				if trigger == "" {
					trigger = "chat"
				} else if before, _, found := strings.Cut(trigger, ":"); found {
					trigger = before
				}
				title := t.Title
				if title == "" {
					title = t.Goal
				}
				root = rootTask{id: t.ID, title: title, trigger: trigger, project: t.ProjectID}
				break
			}
			id = t.Parent
		}
		out[item.ID] = root
	}
	return out
}

type usageWindow struct {
	from     time.Time
	interval string
	starts   []time.Time
	keys     []string
	totals   usageTotals
}

func newUsageWindow(now time.Time, days int, interval string) *usageWindow {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	from := today.AddDate(0, 0, 1-days)
	w := &usageWindow{from: from, interval: interval, totals: newUsageTotals(from, now)}
	for at := from; !at.After(now); {
		w.starts = append(w.starts, at)
		w.keys = append(w.keys, at.Format(time.RFC3339))
		if interval == "hour" {
			at = at.Add(time.Hour)
		} else {
			at = at.AddDate(0, 0, 1)
		}
	}
	return w
}

type usageSample struct {
	root                           rootTask
	agent, harness, project, model string
	row                            UsageRow
	measurement                    usageMeasurement
}
type usageSpan struct{ start, end time.Time }
type rateSpan struct {
	usageSpan
	rate float64
}
type usageMeasurement struct {
	usageSpan
	rate  float64
	valid bool
}

func measureUsage(start, end, to time.Time, tokens int64) usageMeasurement {
	valid := !start.IsZero() && !end.IsZero() && !end.Before(start) && !start.After(to)
	originalSeconds := end.Sub(start).Seconds()
	if end.After(to) {
		end = to
	}
	m := usageMeasurement{usageSpan: usageSpan{start: start, end: end}, valid: valid}
	if valid && end.After(start) && originalSeconds > 0 && tokens > 0 {
		m.rate = float64(tokens) / originalSeconds
	}
	return m
}

type taskUsageDetail struct {
	root                                rootTask
	row                                 UsageRow
	agents, harnesses, models, projects usageKeys
}
type taskWork struct {
	first  *usageMeasurement
	spans  []*usageMeasurement
	detail int
}
type usageGroup struct {
	row     UsageRow
	samples []*usageSample
	// Only the overall total emits task rows. Dimension and time groups
	// borrow the samples, without copies of task metadata or task maps.
	details bool
}
type usageTotals struct {
	from, to                                                  time.Time
	total                                                     *usageGroup
	byTime, byAgent, byModel, byHarness, byTrigger, byProject map[string]*usageGroup
}

func newUsageTotals(from, to time.Time) usageTotals {
	return usageTotals{from: from, to: to, total: &usageGroup{details: true}, byTime: map[string]*usageGroup{}, byAgent: map[string]*usageGroup{}, byModel: map[string]*usageGroup{}, byHarness: map[string]*usageGroup{}, byTrigger: map[string]*usageGroup{}, byProject: map[string]*usageGroup{}}
}

func (u *usageTotals) add(key string, sample *usageSample) {
	u.total.add(sample)
	for _, dimension := range []struct {
		groups map[string]*usageGroup
		key    string
	}{
		{u.byTime, key}, {u.byAgent, sample.agent}, {u.byModel, sample.model}, {u.byHarness, sample.harness}, {u.byTrigger, sample.root.trigger}, {u.byProject, sample.project},
	} {
		group := dimension.groups[dimension.key]
		if group == nil {
			group = &usageGroup{}
			dimension.groups[dimension.key] = group
		}
		group.add(sample)
	}
}

func (g *usageGroup) add(sample *usageSample) {
	g.row = addUsageRow(g.row, sample.row)
	g.samples = append(g.samples, sample)
}

func addUsageRow(row, value UsageRow) UsageRow {
	row.Tokens = row.Tokens.add(value.Tokens)
	row.Seconds += value.Seconds
	row.Attempts += value.Attempts
	row.Unreported += value.Unreported
	return row
}

func (u usageTotals) rows(groups map[string]*usageGroup, scratch *usageScratch) []UsageRow {
	out := make([]UsageRow, 0, len(groups))
	for key, group := range groups {
		row := group.row
		row.Key = key
		stats, _ := group.taskSummary(scratch)
		row.Tasks = &stats
		row.TPM = group.throughput(u.from, u.to, group.curve(scratch), scratch).ActiveTPM
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (g *usageGroup) taskSummary(scratch *usageScratch) (TaskDurationStats, []TaskUsageRow) {
	// Summarize one group at a time. All groups share this temporary map;
	// returned rows contain values, never pointers into it or the details.
	clear(scratch.tasks)
	scratch.details = scratch.details[:0]
	for _, sample := range g.samples {
		if sample.root.id == "" {
			continue
		}
		work, exists := scratch.tasks[sample.root.id]
		if !exists && g.details {
			work.detail = len(scratch.details)
			scratch.details = append(scratch.details, taskUsageDetail{root: sample.root})
		}
		m := &sample.measurement
		if m.valid {
			if work.first == nil {
				work.first = m
			} else {
				work.spans = append(work.spans, m)
			}
		}
		scratch.tasks[sample.root.id] = work
		if g.details {
			detail := &scratch.details[work.detail]
			detail.row = addUsageRow(detail.row, sample.row)
			detail.agents.add(sample.agent)
			detail.harnesses.add(sample.harness)
			detail.models.add(sample.model)
			detail.projects.add(sample.project)
		}
	}
	stats := TaskDurationStats{Count: len(scratch.tasks)}
	var rows []TaskUsageRow
	if g.details {
		rows = make([]TaskUsageRow, 0, len(scratch.tasks))
	}
	for id, work := range scratch.tasks {
		var seconds float64
		if work.first != nil {
			seconds = work.first.end.Sub(work.first.start).Seconds()
			if len(work.spans) > 0 {
				scratch.spans = append(scratch.spans[:0], work.first.usageSpan)
				for _, m := range work.spans {
					scratch.spans = append(scratch.spans, m.usageSpan)
				}
				seconds = unionSeconds(scratch.spans)
			}
			if stats.Measured == 0 || seconds < stats.MinSeconds {
				stats.MinSeconds = seconds
			}
			if seconds > stats.MaxSeconds {
				stats.MaxSeconds = seconds
			}
			stats.Measured++
			stats.TotalSeconds += seconds
		}
		if !g.details {
			continue
		}
		detail := &scratch.details[work.detail]
		row := detail.row
		row.Key = id
		row.Seconds = int64(seconds)
		row.Tasks = &TaskDurationStats{Count: 1}
		if work.first != nil {
			row.Tasks.Measured = 1
			row.Tasks.MinSeconds = seconds
			row.Tasks.MaxSeconds = seconds
			row.Tasks.AverageSeconds = seconds
			row.Tasks.TotalSeconds = seconds
		}
		rows = append(rows, TaskUsageRow{UsageRow: row, TaskID: id, Title: detail.root.title, Trigger: detail.root.trigger, Project: detail.projects.joined(),
			Agent: detail.agents.joined(), Harness: detail.harnesses.joined(), Model: detail.models.joined(), ElapsedSeconds: seconds})
	}
	if stats.Measured > 0 {
		stats.AverageSeconds = stats.TotalSeconds / float64(stats.Measured)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	return stats, rows
}

// Most task trees have one value per dimension. Allocate a set only when
// another distinct value is observed, without changing sorted join semantics.
type usageKeys struct {
	first string
	more  map[string]bool
}

func (k *usageKeys) add(value string) {
	if value == "" || value == k.first {
		return
	}
	if k.first == "" {
		k.first = value
		return
	}
	if k.more == nil {
		k.more = map[string]bool{}
	}
	k.more[value] = true
}

func (k usageKeys) joined() string {
	if len(k.more) == 0 {
		return k.first
	}
	keys := make([]string, 0, 1+len(k.more))
	keys = append(keys, k.first)
	for key := range k.more {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, " · ")
}

// unionSeconds sorts only request-local scratch spans, never input records or
// intervals retained by a task or group.
func unionSeconds(spans []usageSpan) float64 {
	if len(spans) == 0 {
		return 0
	}
	if len(spans) == 1 {
		return spans[0].end.Sub(spans[0].start).Seconds()
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	start, end := spans[0].start, spans[0].end
	var seconds float64
	for _, span := range spans[1:] {
		if span.start.After(end) {
			seconds += end.Sub(start).Seconds()
			start, end = span.start, span.end
		} else if span.end.After(end) {
			end = span.end
		}
	}
	return seconds + end.Sub(start).Seconds()
}

func (g *usageGroup) throughput(from, to time.Time, curve usageCurve, scratch *usageScratch) Throughput {
	scratch.spans = scratch.spans[:0]
	for _, sample := range g.samples {
		if m := &sample.measurement; m.valid && m.end.After(m.start) {
			scratch.spans = append(scratch.spans, m.usageSpan)
		}
	}
	out := Throughput{Estimated: true, ActiveSeconds: unionSeconds(scratch.spans), MeasuredTokens: curve.at(to), PeakTPM: curve.peak}
	out.UnmeasuredTokens = max(0, float64(g.row.Tokens.Total)-out.MeasuredTokens)
	if from.IsZero() {
		for _, span := range scratch.spans {
			if from.IsZero() || span.start.Before(from) {
				from = span.start
			}
		}
	}
	if !from.IsZero() && to.After(from) {
		out.WindowTPM = out.MeasuredTokens / to.Sub(from).Minutes()
	}
	if out.ActiveSeconds > 0 {
		out.ActiveTPM = out.MeasuredTokens / (out.ActiveSeconds / 60)
	}
	return out
}

// The curve integrates simultaneous per-execution rates. Peak calculation
// touches only event-boundary minutes; whole spans need no minute allocation.
// Runtime and memory depend on execution count, not fleet history length.
type usageEvent struct {
	at     time.Time
	delta  float64
	active int
}
type curvePoint struct {
	at           time.Time
	tokens, rate float64
}
type usageCurve struct {
	points []curvePoint
	peak   float64
}

// Scratch belongs to one usage call. Curves borrow points until the next curve
// is built; only scalar throughput values and independently owned rows escape.
type usageScratch struct {
	spans   []usageSpan
	events  []usageEvent
	points  []curvePoint
	tasks   map[string]taskWork
	details []taskUsageDetail
}

func (g *usageGroup) curve(scratch *usageScratch) usageCurve {
	scratch.events = scratch.events[:0]
	for _, sample := range g.samples {
		if m := &sample.measurement; m.rate > 0 {
			scratch.events = append(scratch.events, usageEvent{at: m.start, delta: m.rate, active: 1}, usageEvent{at: m.end, delta: -m.rate, active: -1})
		}
	}
	return scratch.curve()
}

func makeUsageCurve(spans []rateSpan) usageCurve {
	scratch := usageScratch{events: make([]usageEvent, 0, 2*len(spans))}
	for _, span := range spans {
		scratch.events = append(scratch.events, usageEvent{at: span.start, delta: span.rate, active: 1}, usageEvent{at: span.end, delta: -span.rate, active: -1})
	}
	return scratch.curve()
}

func (scratch *usageScratch) curve() usageCurve {
	events := scratch.events
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	if cap(scratch.points) < len(events) {
		scratch.points = make([]curvePoint, 0, len(events))
	}
	curve := usageCurve{points: scratch.points[:0]}
	// Sorted events visit boundary minutes in order. Finalize a minute when
	// moving to the next one instead of retaining a map of the whole history.
	var minute int64
	var minuteTokens float64
	addMinute := func(at time.Time, amount float64) {
		if key := at.Unix(); key != minute {
			curve.peak = max(curve.peak, minuteTokens)
			minute, minuteTokens = key, 0
		}
		minuteTokens += amount
	}
	var previous time.Time
	var tokens, rate float64
	active := 0
	for i := 0; i < len(events); {
		at := events[i].at
		if !previous.IsZero() && at.After(previous) && rate > 0 {
			tokens += rate * at.Sub(previous).Seconds()
			first, last := previous.Truncate(time.Minute), at.Truncate(time.Minute)
			if first.Equal(last) {
				addMinute(first, rate*at.Sub(previous).Seconds())
			} else {
				addMinute(first, rate*first.Add(time.Minute).Sub(previous).Seconds())
				addMinute(last, rate*at.Sub(last).Seconds())
				if last.After(first.Add(time.Minute)) {
					curve.peak = max(curve.peak, rate*60)
				}
			}
		}
		for i < len(events) && events[i].at.Equal(at) {
			rate += events[i].delta
			active += events[i].active
			i++
		}
		// Floating cancellation of rates can leave a tiny negative residue.
		rate = max(0, rate)
		if active == 0 {
			rate = 0
		}
		curve.points = append(curve.points, curvePoint{at: at, tokens: tokens, rate: rate})
		previous = at
	}
	curve.peak = max(curve.peak, minuteTokens)
	scratch.points = curve.points
	return curve
}

func (c usageCurve) at(at time.Time) float64 {
	i := sort.Search(len(c.points), func(i int) bool { return c.points[i].at.After(at) }) - 1
	if i < 0 {
		return 0
	}
	point := c.points[i]
	return point.tokens + max(0, point.rate*at.Sub(point.at).Seconds())
}
