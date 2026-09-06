package schedule

import (
	"fmt"
	"sort"
	"time"
)

const (
	FiringPending     = "pending"
	FiringDispatching = "dispatching"
	FiringAccepted    = "accepted"
	FiringUnknown     = "unknown"
	FiringCancelled   = "cancelled"
	FiringSkipped     = "skipped"
)

// Firing freezes one scheduled occurrence. Claiming it does not consume the
// job; only durable acceptance, or an explicit skip, moves the schedule on.
type Firing struct {
	Job         `json:"job"`
	Key         string    `json:"key"`
	ScheduledAt time.Time `json:"scheduled_at"`
	FollowingAt time.Time `json:"following_at,omitzero"`
	State       string    `json:"state"`
	Receipt     string    `json:"receipt,omitempty"`
	Error       string    `json:"error,omitempty"`
	AcceptedAt  time.Time `json:"accepted_at,omitzero"`
	ResolvedBy  string    `json:"resolved_by,omitempty"`
}

func firingKey(id string, at time.Time) string {
	return "schedule:" + id + ":" + at.UTC().Format(time.RFC3339Nano)
}

func (s *Store) recoverFirings() error {
	next := s.clone()
	changed := false
	for _, f := range next.Firings {
		if f.State != FiringDispatching {
			continue
		}
		changed = true
		if f.Channel == "console" {
			f.State, f.Error = FiringPending, "restart: retrying the same durable console submission"
		} else {
			f.State, f.Error = FiringUnknown, "restart before delivery was confirmed; inspect the channel before confirming or retrying"
		}
	}
	if changed {
		return s.replaceLocked(next)
	}
	return nil
}

// Due materializes due occurrences and also returns pending deliveries from
// earlier ticks. BeginFiring is the exclusive dispatch transition.
func (s *Store) Due(now time.Time) ([]Firing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.clone()
	var due []Firing
	changed := false
	// A failed receipt write can leave a dispatch record after its sender
	// has returned. Recover it with the same policy used after restart.
	for key, f := range next.Firings {
		if f.State != FiringDispatching || s.active[key] {
			continue
		}
		changed = true
		if f.Channel == "console" {
			f.State, f.Error = FiringPending, "delivery receipt was not saved; retrying the same console submission"
		} else {
			f.State, f.Error = FiringUnknown, "delivery receipt was not saved; inspect the channel before confirming or retrying"
		}
	}
	for id, job := range next.Jobs {
		var outstanding *Firing
		for _, f := range next.Firings {
			if f.ID == id && (f.State == FiringPending || f.State == FiringDispatching || f.State == FiringUnknown) {
				outstanding = f
				break
			}
		}
		if outstanding != nil {
			if outstanding.State == FiringPending {
				due = append(due, *outstanding)
			}
			continue
		}
		if job.NextAt.IsZero() || job.NextAt.After(now) {
			continue
		}
		f := Firing{Job: *job, Key: firingKey(id, job.NextAt), ScheduledAt: job.NextAt, FollowingAt: job.Spec.Next(now), State: FiringPending}
		changed = true
		if now.Sub(job.NextAt) > MaxLateness {
			f.State, f.Error = FiringSkipped, "scheduled occurrence exceeded maximum lateness"
			advanceJob(next.Jobs, id, f.FollowingAt, now, false)
		} else {
			due = append(due, f)
		}
		next.Firings[f.Key] = &f
	}
	if changed {
		if err := s.replaceLocked(next); err != nil {
			return nil, err
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ID == due[j].ID {
			return due[i].ScheduledAt.Before(due[j].ScheduledAt)
		}
		return lessID(due[i].ID, due[j].ID)
	})
	return due, nil
}

func (s *Store) BeginFiring(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.data.Firings[key]
	if !ok {
		return fmt.Errorf("firing %s not found", key)
	}
	if f.State != FiringPending || s.active[key] {
		return fmt.Errorf("firing %s is %s", key, f.State)
	}
	next := s.clone()
	next.Firings[key].State = FiringDispatching
	if err := s.replaceLocked(next); err != nil {
		return err
	}
	s.active[key] = true
	return nil
}

func (s *Store) AcceptFiring(key, receipt string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, key)
	return s.acceptLocked(key, receipt, at, "")
}

func (s *Store) acceptLocked(key, receipt string, at time.Time, by string) error {
	if receipt == "" {
		return fmt.Errorf("firing acceptance requires a receipt")
	}
	f, ok := s.data.Firings[key]
	if !ok {
		return fmt.Errorf("firing %s not found", key)
	}
	if f.State == FiringAccepted {
		if f.Receipt == receipt {
			return nil
		}
		return fmt.Errorf("firing %s already has another receipt", key)
	}
	if f.State != FiringDispatching && !(f.State == FiringUnknown && by != "") {
		return fmt.Errorf("firing %s is %s", key, f.State)
	}
	next := s.clone()
	stored := next.Firings[key]
	stored.State, stored.Receipt, stored.AcceptedAt, stored.ResolvedBy = FiringAccepted, receipt, at, by
	stored.Error = ""
	advanceJob(next.Jobs, f.ID, f.FollowingAt, at, true)
	return s.replaceLocked(next)
}

func advanceJob(jobs map[string]*Job, id string, followingAt, now time.Time, accepted bool) {
	job, ok := jobs[id]
	if !ok {
		return
	}
	if accepted {
		job.Runs++
		job.LastAt = now
	}
	if !job.Spec.Recurring() {
		delete(jobs, id)
		return
	}
	job.NextAt = followingAt
	if job.NextAt.IsZero() {
		delete(jobs, id)
	}
}

func (s *Store) FailFiring(key string, cause error, unknown bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer delete(s.active, key)
	f, ok := s.data.Firings[key]
	if !ok {
		return fmt.Errorf("firing %s not found", key)
	}
	if f.State != FiringDispatching {
		return fmt.Errorf("firing %s is %s", key, f.State)
	}
	next := s.clone()
	f = next.Firings[key]
	f.State = FiringPending
	if unknown {
		f.State = FiringUnknown
	}
	if cause != nil {
		f.Error = cause.Error()
	}
	return s.replaceLocked(next)
}

// ResolveFiring is an explicit human decision after inspecting an uncertain
// external delivery. Active requests cannot be resolved underneath a sender.
func (s *Store) ResolveFiring(id, verdict, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if by == "" {
		return fmt.Errorf("firing resolution requires an actor")
	}
	for key, f := range s.data.Firings {
		if f.ID != id || f.State != FiringUnknown {
			continue
		}
		if s.active[key] {
			return fmt.Errorf("firing %s is still active", key)
		}
		switch verdict {
		case "confirm":
			return s.acceptLocked(key, "confirmed:"+by, s.now(), by)
		case "retry":
			next := s.clone()
			next.Firings[key].State = FiringPending
			next.Firings[key].ResolvedBy = by
			return s.replaceLocked(next)
		default:
			return fmt.Errorf("use confirm or retry for an uncertain schedule")
		}
	}
	return fmt.Errorf("schedule %s has no uncertain firing", id)
}

func (s *Store) describeLocked(job Job) Job {
	job.State = "scheduled"
	var latest *Firing
	for _, f := range s.data.Firings {
		if f.ID == job.ID && (latest == nil || f.ScheduledAt.After(latest.ScheduledAt)) {
			latest = f
		}
	}
	if latest != nil {
		job.State, job.Error = latest.State, latest.Error
		if latest.State == FiringPending || latest.State == FiringDispatching || latest.State == FiringUnknown {
			job.PendingKey = latest.Key
		}
	}
	return job
}
