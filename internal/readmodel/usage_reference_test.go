package readmodel

// Frozen pre-optimization implementation. Keep independent of production helpers
// so differential tests detect changes in every exported usage field.

import (
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
)

// referenceUsage counts only the ledger's closed attempts. Tasks supply ownership and
// trigger metadata; their cached token totals never enter this accounting.
func referenceUsage(closed []attempt.Record, now time.Time, tasks []Task) Usage {
	roots := referenceTaskRoots(tasks)
	all := referenceNewUsageTotals(time.Time{}, now)
	periods := map[string]*referenceUsageWindow{
		"1d": referenceNewUsageWindow(now, 1, "hour"), "7d": referenceNewUsageWindow(now, 7, "day"), "30d": referenceNewUsageWindow(now, 30, "day"),
	}
	for _, r := range closed {
		sample := referenceUsageSample{record: r, root: roots[r.TaskID]}
		if r.TaskID != "" && sample.root.id == "" {
			sample.root.id = r.TaskID
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
			window.totals.add(window.starts[i].Format(time.RFC3339), sample)
		}
	}
	stats, byTask := all.total.taskSummary()
	result := Usage{ByDay: all.rows(all.byTime), ByAgent: all.rows(all.byAgent), ByModel: all.rows(all.byModel), Total: all.total.row,
		ByHarness: all.rows(all.byHarness), ByTrigger: all.rows(all.byTrigger), ByProject: all.rows(all.byProject), Tasks: stats, ByTask: byTask,
		Throughput: all.total.throughput(all.from, now), Timezone: now.Location().String(), Periods: make(map[string]UsagePeriod, len(periods))}
	result.Total.Key = "total"
	for key, window := range periods {
		u := window.totals
		series := make([]UsageRow, 0, len(window.starts))
		curve := referenceMakeUsageCurve(u.total.rates)
		for i, at := range window.starts {
			key := at.Format(time.RFC3339)
			row := UsageRow{Key: key}
			if group := u.byTime[key]; group != nil {
				row = group.row
				row.Key = key
				stats, _ := group.taskSummary()
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
		stats, byTask := u.total.taskSummary()
		total := u.total.row
		total.Key = "total"
		result.Periods[key] = UsagePeriod{From: window.from, To: now, Interval: window.interval, Series: series,
			ByAgent: u.rows(u.byAgent), ByModel: u.rows(u.byModel), Total: total,
			ByHarness: u.rows(u.byHarness), ByTrigger: u.rows(u.byTrigger), ByProject: u.rows(u.byProject), Tasks: stats, ByTask: byTask,
			Throughput: u.total.throughput(window.from, now)}
	}
	return result
}

type referenceRootTask struct{ id, title, trigger, project string }

func referenceTaskRoots(tasks []Task) map[string]referenceRootTask {
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	out := make(map[string]referenceRootTask, len(tasks))
	for _, item := range tasks {
		root := referenceRootTask{id: item.ID}
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
				root = referenceRootTask{id: t.ID, title: title, trigger: trigger, project: t.ProjectID}
				break
			}
			id = t.Parent
		}
		out[item.ID] = root
	}
	return out
}

type referenceUsageWindow struct {
	from     time.Time
	interval string
	starts   []time.Time
	totals   referenceUsageTotals
}

func referenceNewUsageWindow(now time.Time, days int, interval string) *referenceUsageWindow {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	from := today.AddDate(0, 0, 1-days)
	w := &referenceUsageWindow{from: from, interval: interval, totals: referenceNewUsageTotals(from, now)}
	for at := from; !at.After(now); {
		w.starts = append(w.starts, at)
		if interval == "hour" {
			at = at.Add(time.Hour)
		} else {
			at = at.AddDate(0, 0, 1)
		}
	}
	return w
}

type referenceUsageSample struct {
	record attempt.Record
	root   referenceRootTask
	model  string
	row    UsageRow
}
type referenceUsageSpan struct{ start, end time.Time }
type referenceRateSpan struct {
	referenceUsageSpan
	rate float64
}
type referenceTaskUsageDetail struct {
	root                                referenceRootTask
	row                                 UsageRow
	agents, harnesses, models, projects map[string]bool
}
type referenceTaskWork struct {
	spans    []referenceUsageSpan
	measured bool
	detail   *referenceTaskUsageDetail
}
type referenceUsageGroup struct {
	row   UsageRow
	tasks map[string]*referenceTaskWork
	spans []referenceUsageSpan
	rates []referenceRateSpan
	// Only the overall total emits task rows. Dimension and time groups
	// retain the duration samples, without copies of task metadata.
	details bool
}
type referenceUsageTotals struct {
	from, to                                                  time.Time
	total                                                     *referenceUsageGroup
	byTime, byAgent, byModel, byHarness, byTrigger, byProject map[string]*referenceUsageGroup
}

func referenceNewUsageTotals(from, to time.Time) referenceUsageTotals {
	return referenceUsageTotals{from: from, to: to, total: &referenceUsageGroup{details: true}, byTime: map[string]*referenceUsageGroup{}, byAgent: map[string]*referenceUsageGroup{}, byModel: map[string]*referenceUsageGroup{}, byHarness: map[string]*referenceUsageGroup{}, byTrigger: map[string]*referenceUsageGroup{}, byProject: map[string]*referenceUsageGroup{}}
}

func (u *referenceUsageTotals) add(key string, sample referenceUsageSample) {
	u.total.add(sample, u.from, u.to)
	project := sample.record.Project
	if project == "" {
		project = sample.root.project
	}
	for _, dimension := range []struct {
		groups map[string]*referenceUsageGroup
		key    string
	}{
		{u.byTime, key}, {u.byAgent, sample.record.Agent}, {u.byModel, sample.model}, {u.byHarness, sample.record.Harness}, {u.byTrigger, sample.root.trigger}, {u.byProject, project},
	} {
		group := dimension.groups[dimension.key]
		if group == nil {
			group = &referenceUsageGroup{}
			dimension.groups[dimension.key] = group
		}
		group.add(sample, u.from, u.to)
	}
}

func (g *referenceUsageGroup) add(sample referenceUsageSample, from, to time.Time) {
	g.row = referenceAddUsageRow(g.row, sample.row)
	start, end := sample.record.StartedAt, sample.record.EndedAt
	valid := !start.IsZero() && !end.IsZero() && !end.Before(start) && !start.After(to)
	originalSeconds := end.Sub(start).Seconds()
	if end.After(to) {
		end = to
	}
	if !from.IsZero() && start.Before(from) {
		start = from
	}
	valid = valid && !end.Before(start)
	span := referenceUsageSpan{start: start, end: end}
	if valid && end.After(start) {
		g.spans = append(g.spans, span)
		if originalSeconds > 0 && sample.row.Tokens.Total > 0 {
			g.rates = append(g.rates, referenceRateSpan{referenceUsageSpan: span, rate: float64(sample.row.Tokens.Total) / originalSeconds})
		}
	}
	if sample.root.id == "" {
		return
	}
	if g.tasks == nil {
		g.tasks = map[string]*referenceTaskWork{}
	}
	work := g.tasks[sample.root.id]
	if work == nil {
		work = &referenceTaskWork{}
		if g.details {
			work.detail = &referenceTaskUsageDetail{root: sample.root, agents: map[string]bool{}, harnesses: map[string]bool{}, models: map[string]bool{}, projects: map[string]bool{}}
		}
		g.tasks[sample.root.id] = work
	}
	if valid {
		work.measured = true
		work.spans = append(work.spans, span)
	}
	if work.detail == nil {
		return
	}
	detail := work.detail
	detail.row = referenceAddUsageRow(detail.row, sample.row)
	if sample.record.Agent != "" {
		detail.agents[sample.record.Agent] = true
	}
	if sample.record.Harness != "" {
		detail.harnesses[sample.record.Harness] = true
	}
	if sample.model != "" {
		detail.models[sample.model] = true
	}
	project := sample.record.Project
	if project == "" {
		project = sample.root.project
	}
	if project != "" {
		detail.projects[project] = true
	}
}

func referenceAddUsageRow(row, value UsageRow) UsageRow {
	row.Tokens = row.Tokens.add(value.Tokens)
	row.Seconds += value.Seconds
	row.Attempts += value.Attempts
	row.Unreported += value.Unreported
	return row
}

func (u referenceUsageTotals) rows(groups map[string]*referenceUsageGroup) []UsageRow {
	out := make([]UsageRow, 0, len(groups))
	for key, group := range groups {
		row := group.row
		row.Key = key
		stats, _ := group.taskSummary()
		row.Tasks = &stats
		row.TPM = group.throughput(u.from, u.to).ActiveTPM
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (g *referenceUsageGroup) taskSummary() (TaskDurationStats, []TaskUsageRow) {
	stats := TaskDurationStats{Count: len(g.tasks)}
	var rows []TaskUsageRow
	if g.details {
		rows = make([]TaskUsageRow, 0, len(g.tasks))
	}
	for id, work := range g.tasks {
		seconds := referenceUnionSeconds(work.spans)
		if work.measured {
			if stats.Measured == 0 || seconds < stats.MinSeconds {
				stats.MinSeconds = seconds
			}
			if seconds > stats.MaxSeconds {
				stats.MaxSeconds = seconds
			}
			stats.Measured++
			stats.TotalSeconds += seconds
		}
		if work.detail == nil {
			continue
		}
		detail := work.detail
		row := detail.row
		row.Key = id
		row.Seconds = int64(seconds)
		row.Tasks = &TaskDurationStats{Count: 1}
		if work.measured {
			row.Tasks.Measured = 1
			row.Tasks.MinSeconds = seconds
			row.Tasks.MaxSeconds = seconds
			row.Tasks.AverageSeconds = seconds
			row.Tasks.TotalSeconds = seconds
		}
		rows = append(rows, TaskUsageRow{UsageRow: row, TaskID: id, Title: detail.root.title, Trigger: detail.root.trigger, Project: referenceJoinedKeys(detail.projects),
			Agent: referenceJoinedKeys(detail.agents), Harness: referenceJoinedKeys(detail.harnesses), Model: referenceJoinedKeys(detail.models), ElapsedSeconds: seconds})
	}
	if stats.Measured > 0 {
		stats.AverageSeconds = stats.TotalSeconds / float64(stats.Measured)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	return stats, rows
}

func referenceJoinedKeys(values map[string]bool) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, " · ")
}

func referenceUnionSeconds(spans []referenceUsageSpan) float64 {
	if len(spans) == 0 {
		return 0
	}
	if len(spans) == 1 {
		return spans[0].end.Sub(spans[0].start).Seconds()
	}
	ordered := append([]referenceUsageSpan(nil), spans...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start.Before(ordered[j].start) })
	start, end := ordered[0].start, ordered[0].end
	var seconds float64
	for _, span := range ordered[1:] {
		if span.start.After(end) {
			seconds += end.Sub(start).Seconds()
			start, end = span.start, span.end
		} else if span.end.After(end) {
			end = span.end
		}
	}
	return seconds + end.Sub(start).Seconds()
}

func (g *referenceUsageGroup) throughput(from, to time.Time) Throughput {
	curve := referenceMakeUsageCurve(g.rates)
	out := Throughput{Estimated: true, ActiveSeconds: referenceUnionSeconds(g.spans), MeasuredTokens: curve.at(to), PeakTPM: curve.peak}
	out.UnmeasuredTokens = max(0, float64(g.row.Tokens.Total)-out.MeasuredTokens)
	if from.IsZero() {
		for _, span := range g.spans {
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
type referenceUsageEvent struct {
	at     time.Time
	delta  float64
	active int
}
type referenceCurvePoint struct {
	at           time.Time
	tokens, rate float64
}
type referenceUsageCurve struct {
	points []referenceCurvePoint
	peak   float64
}

func referenceMakeUsageCurve(spans []referenceRateSpan) referenceUsageCurve {
	events := make([]referenceUsageEvent, 0, 2*len(spans))
	for _, span := range spans {
		events = append(events, referenceUsageEvent{at: span.start, delta: span.rate, active: 1}, referenceUsageEvent{at: span.end, delta: -span.rate, active: -1})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	curve := referenceUsageCurve{}
	boundaryMinutes := map[int64]float64{}
	var previous time.Time
	var tokens, rate float64
	active := 0
	for i := 0; i < len(events); {
		at := events[i].at
		if !previous.IsZero() && at.After(previous) && rate > 0 {
			tokens += rate * at.Sub(previous).Seconds()
			first, last := previous.Truncate(time.Minute), at.Truncate(time.Minute)
			if first.Equal(last) {
				boundaryMinutes[first.Unix()] += rate * at.Sub(previous).Seconds()
			} else {
				boundaryMinutes[first.Unix()] += rate * first.Add(time.Minute).Sub(previous).Seconds()
				boundaryMinutes[last.Unix()] += rate * at.Sub(last).Seconds()
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
		curve.points = append(curve.points, referenceCurvePoint{at: at, tokens: tokens, rate: rate})
		previous = at
	}
	for _, amount := range boundaryMinutes {
		curve.peak = max(curve.peak, amount)
	}
	return curve
}

func (c referenceUsageCurve) at(at time.Time) float64 {
	i := sort.Search(len(c.points), func(i int) bool { return c.points[i].at.After(at) }) - 1
	if i < 0 {
		return 0
	}
	point := c.points[i]
	return point.tokens + max(0, point.rate*at.Sub(point.at).Seconds())
}
