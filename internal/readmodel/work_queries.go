package readmodel

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

type PlanCoverage struct {
	Total    int  `json:"total"`
	Included int  `json:"included"`
	HasMore  bool `json:"has_more"`
}

type TaskPage struct {
	Items      []Task `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type PlanPage struct {
	Items      []Plan `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type AccountingItem struct {
	Index       int    `json:"index"`
	ExecutionID string `json:"execution_id,omitempty"`
	AttemptRow
}

type AccountingPage struct {
	Items      []AccountingItem `json:"items"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type TaskDetail struct {
	Task       Task           `json:"task"`
	Plan       *Plan          `json:"plan,omitempty"`
	Children   TaskPage       `json:"children"`
	Accounting AccountingPage `json:"accounting"`
}

func (m *Model) TaskAccounting(id, cursor string, limit int) (AccountingPage, error) {
	if m.src.Tasks == nil {
		return AccountingPage{}, errors.New("tasks are not wired")
	}
	page, err := m.src.Tasks.QueryAttempts(id, cursor, limit)
	if err != nil {
		return AccountingPage{}, err
	}
	out := AccountingPage{Items: make([]AccountingItem, 0, len(page.Items)), Total: page.Total, NextCursor: page.NextCursor}
	for _, row := range page.Items {
		a := row.Attempt
		item := AccountingItem{Index: row.Index, ExecutionID: a.ExecutionID, AttemptRow: AttemptRow{
			Day: a.StartedAt.UTC().Format("2006-01-02"), Agent: a.Member, Node: a.Node, Model: a.Model, Outcome: string(a.Outcome), Started: a.StartedAt,
			Tokens:   Tokens{Input: a.Tokens.Input, Output: a.Tokens.Output, CachedRead: a.Tokens.CachedRead, CachedWrite: a.Tokens.CachedWrite, Total: a.Tokens.Total},
			Reported: a.Tokens.Input+a.Tokens.Output+a.Tokens.CachedRead > 0,
		}}
		if !a.EndedAt.IsZero() {
			item.Seconds = int64(a.EndedAt.Sub(a.StartedAt).Seconds())
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// projectSelected keeps the query's owner headers and ordering. Standing live
// signals and their ancestor closure provide axes; truncated historic children
// never determine completion eligibility, which comes from the owner summary.
func (m *Model) projectSelected(ctx context.Context, headers []task.Header) ([]Task, map[string]plan.Plan, error) {
	b := &snapshotBuilder{m: m, text: i18n.New(i18n.ContextLocale(ctx))}
	b.sources()
	b.liveAttempts(ctx)
	b.ledgerFacts(ctx)
	b.inbox()
	ids := make([]string, 0, len(headers))
	for _, h := range headers {
		ids = append(ids, h.ID)
	}
	b.plansAndTasks(ids)
	selected := projectTasks(headers, b.planByTask)
	byID := map[string]int{}
	for i, item := range selected {
		byID[item.ID] = i
	}
	for i, item := range b.snap.Tasks {
		if at, ok := byID[item.ID]; ok {
			p := selected[at]
			p.Children = item.Children
			p.ChildrenComplete = len(p.Children) == p.ChildrenCount
			b.snap.Tasks[i] = p
		}
	}
	b.taskAxes()
	for _, item := range b.snap.Tasks {
		if at, ok := byID[item.ID]; ok {
			selected[at] = item
		}
	}
	for _, source := range b.snap.Sources {
		if source.Error != "" {
			return nil, nil, fmt.Errorf("task projection source %s: %s", source.Name, source.Error)
		}
	}
	return selected, b.planByTask, nil
}

func (m *Model) TaskHistory(ctx context.Context, q task.Query) (TaskPage, error) {
	if m.src.Tasks == nil {
		return TaskPage{}, errors.New("tasks are not wired")
	}
	page, err := m.src.Tasks.Query(q)
	if err != nil {
		return TaskPage{}, err
	}
	items, _, err := m.projectSelected(ctx, page.Items)
	if err != nil {
		return TaskPage{}, err
	}
	return TaskPage{Items: items, Total: page.Total, NextCursor: page.NextCursor}, nil
}

// TaskDetail is a point read, independent of base snapshot membership. Child
// and accounting histories each carry their own explicit bounded page cursor.
func (m *Model) TaskDetail(ctx context.Context, id string) (TaskDetail, error) {
	if m.src.Tasks == nil {
		return TaskDetail{}, errors.New("tasks are not wired")
	}
	head, exists := m.src.Tasks.Header(id)
	if !exists {
		return TaskDetail{}, task.ErrTaskNotFound
	}
	children, err := m.src.Tasks.Query(task.Query{Scope: task.Scope{Kind: "children", ID: id}, Limit: 20})
	if err != nil {
		return TaskDetail{}, err
	}
	accounting, err := m.TaskAccounting(id, "", 20)
	if err != nil {
		return TaskDetail{}, err
	}
	items, plans, err := m.projectSelected(ctx, append([]task.Header{head}, children.Items...))
	if err != nil {
		return TaskDetail{}, err
	}
	detail := TaskDetail{Task: items[0], Children: TaskPage{Items: items[1:], Total: children.Total, NextCursor: children.NextCursor}, Accounting: accounting}
	if p, ok := plans[id]; ok {
		converted := convertPlan(p)
		detail.Plan = &converted
	}
	return detail, nil
}

func (m *Model) PlanHistory(q plan.Query) (PlanPage, error) {
	if m.src.Plans == nil {
		return PlanPage{}, errors.New("plans are not wired")
	}
	page, err := m.src.Plans.Query(q)
	if err != nil {
		return PlanPage{}, err
	}
	out := PlanPage{Items: make([]Plan, 0, len(page.Items)), Total: page.Total, NextCursor: page.NextCursor}
	for _, p := range page.Items {
		out.Items = append(out.Items, convertPlan(p))
	}
	return out, nil
}
