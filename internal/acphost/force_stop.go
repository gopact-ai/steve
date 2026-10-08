package acphost

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

type processKiller interface{ KillNow(context.Context) error }

// Kill closes the host permanently and signals every unsettled generation
// immediately. Sending a signal is not exit evidence: each transport must
// still prove that its original process group stopped.
func (h *Host) Kill(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	retry := time.NewTicker(time.Millisecond)
	defer retry.Stop()
	for !h.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
	h.isClosed, h.alive = true, false
	h.cancelFileCallsLocked(0)
	h.retireFileRootsLocked(0)
	conn, stdin := h.conn, h.stdin
	var processes []Process
	for _, p := range h.processes {
		if !p.Stopped() {
			processes = append(processes, p)
		}
	}
	var settled []chan struct{}
	for _, done := range h.settling {
		settled = append(settled, done)
	}
	h.mu.Unlock()
	results := make(chan error, len(processes))
	for _, p := range processes {
		go func() {
			killer, ok := p.(processKiller)
			if !ok {
				results <- procgroup.ErrUnsupported
				return
			}
			results <- killer.KillNow(ctx)
		}()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
	var result error
	for range processes {
		result = errors.Join(result, <-results)
	}
	if result != nil {
		return result
	}
	for _, done := range settled {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(procgroup.ErrRunning, ctx.Err())
		}
	}
	if !h.AllProcessesStopped() {
		return procgroup.ErrRunning
	}
	return nil
}

// KillNow signals while the unreaped child still pins its process-group id.
// After reaping, the recorded identity must be proved again before signalling;
// the old numeric group id alone can belong to an unrelated process.
func (p *localProcess) KillNow(ctx context.Context) error {
	if p.groupUnsupported.Load() {
		return procgroup.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.stopped.Load() {
		p.mu.Unlock()
		return nil
	}
	if !p.reaped {
		err := p.group.kill(p.cmd.Process.Pid)
		p.mu.Unlock()
		return err
	}
	identity, place := p.identity, p.place
	settle := p.group.settle
	p.mu.Unlock()
	if settle == nil {
		settle = procgroup.Settle
	}
	here, err := procgroup.Here()
	if err != nil {
		return err
	}
	within := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		within = min(within, time.Until(deadline))
	}
	err = settle(identity, place, here, within)
	if err == nil {
		p.stopped.Store(true)
	}
	return err
}
