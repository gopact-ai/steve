package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// TaskAttempts are a task's attempts from the ledger, newest first.
func (a *fleetAdmin) TaskAttempts(ctx context.Context, taskID string) ([]consoleapi.AttemptView, error) {
	if a.attempts == nil {
		return nil, errors.New("attempts are not wired")
	}
	records, err := a.attempts.ForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]consoleapi.AttemptView, 0, len(records))
	for _, r := range records {
		v := consoleapi.AttemptView{ID: r.ID, Kind: string(r.Kind), State: string(r.State), Agent: r.Agent, Node: r.Node, Harness: r.Harness,
			Workspace: r.Workspace.Path, Base: r.Base, Error: r.Error, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
		if r.Result != nil {
			v.Artifact, v.Summary = r.Result.Artifact, r.Result.Summary
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	// How many files each changed, for the newest few: one diff-tree each.
	for i := range out {
		if i >= 20 || a.artifacts == nil || out[i].Artifact == "" || out[i].Artifact == out[i].Base {
			continue
		}
		if changes, _, err := a.artifacts.Changes(ctx, records[0].Project, out[i].Base, out[i].Artifact); err == nil {
			out[i].Files = len(changes)
		}
	}
	return out, nil
}

// attemptSnapshot is the snapshot an attempt is browsed at: its result
// when it has one, else what it started from.
func (a *fleetAdmin) attemptSnapshot(ctx context.Context, attemptID string) (attempt.Record, string, string, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return attempt.Record{}, "", "", err
	}
	if after != "" {
		return record, after, "result", nil
	}
	if base == "" {
		return attempt.Record{}, "", "", errors.New("this attempt has no snapshot to browse")
	}
	return record, base, "base", nil
}

// AttemptTree lists a directory of an attempt's snapshot.
func (a *fleetAdmin) AttemptTree(ctx context.Context, attemptID, dir string) (consoleapi.TreeView, error) {
	record, commit, which, err := a.attemptSnapshot(ctx, attemptID)
	if err != nil {
		return consoleapi.TreeView{}, err
	}
	entries, truncated, err := a.artifacts.Tree(ctx, record.Project, commit, dir)
	if err != nil {
		return consoleapi.TreeView{}, err
	}
	return consoleapi.TreeView{Attempt: attemptID, Commit: commit, Which: which, Dir: strings.Trim(dir, "/"), Entries: entries, Truncated: truncated}, nil
}

// AttemptFile reads one file of an attempt's snapshot.
func (a *fleetAdmin) AttemptFile(ctx context.Context, attemptID, path string) (consoleapi.FileView, error) {
	record, commit, _, err := a.attemptSnapshot(ctx, attemptID)
	if err != nil {
		return consoleapi.FileView{}, err
	}
	text, size, binary, truncated, err := a.artifacts.File(ctx, record.Project, commit, path)
	if err != nil {
		return consoleapi.FileView{}, err
	}
	return consoleapi.FileView{Attempt: attemptID, Commit: commit, Path: path, Text: text, Size: size, Binary: binary, Truncated: truncated}, nil
}

// changeSnapshots is the before and after of an attempt, when it captured
// one: nothing changed leaves the after empty.
func (a *fleetAdmin) changeSnapshots(ctx context.Context, attemptID string) (attempt.Record, string, string, error) {
	if a.attempts == nil || a.artifacts == nil {
		return attempt.Record{}, "", "", errors.New("attempts are not wired")
	}
	record, err := a.attempts.Get(ctx, attemptID)
	if err != nil {
		return attempt.Record{}, "", "", err
	}
	after := ""
	if record.Result != nil {
		after = record.Result.Artifact
	}
	return record, record.Base, after, nil
}

// Changes is what an attempt changed, as the reply keeps it.
func (a *fleetAdmin) Changes(ctx context.Context, attemptID string) (*consoleapi.ChangeSummary, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	summary := &consoleapi.ChangeSummary{Attempt: attemptID, Project: record.Project, Base: base, Artifact: after}
	if record.Result != nil && record.Result.CaptureError != "" {
		summary.Note = "改动没有记录：" + record.Result.CaptureError
		return summary, nil
	}
	if after == "" || after == base {
		return summary, nil // nothing changed: the fold says so
	}
	changes, truncated, err := a.artifacts.Changes(ctx, record.Project, base, after)
	if err != nil {
		summary.Note = err.Error()
		return summary, nil
	}
	summary.Files = len(changes)
	if truncated {
		summary.Note = "只统计了前 " + fmt.Sprint(len(changes)) + " 个"
	}
	return summary, nil
}

// AttemptChanges is the files an attempt changed.
func (a *fleetAdmin) AttemptChanges(ctx context.Context, attemptID string) (consoleapi.ChangeIndex, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return consoleapi.ChangeIndex{}, err
	}
	index := consoleapi.ChangeIndex{Attempt: attemptID, Project: record.Project, Base: base, Artifact: after}
	if after == "" || after == base {
		index.Note = "没有改动"
		return index, nil
	}
	changes, truncated, err := a.artifacts.Changes(ctx, record.Project, base, after)
	if err != nil {
		return consoleapi.ChangeIndex{}, err
	}
	index.Changes, index.Truncated = changes, truncated
	return index, nil
}

// AttemptDiff is one changed file's diff.
func (a *fleetAdmin) AttemptDiff(ctx context.Context, attemptID, path string) (consoleapi.FileDiff, error) {
	record, base, after, err := a.changeSnapshots(ctx, attemptID)
	if err != nil {
		return consoleapi.FileDiff{}, err
	}
	if after == "" {
		return consoleapi.FileDiff{}, errors.New("没有改动")
	}
	diff, truncated, err := a.artifacts.FileDiff(ctx, record.Project, base, after, path)
	if err != nil {
		return consoleapi.FileDiff{}, err
	}
	return consoleapi.FileDiff{Path: path, Diff: diff, Truncated: truncated}, nil
}
