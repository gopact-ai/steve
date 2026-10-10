package procgroup

import (
	"fmt"
	"time"
)

// Preparation names an inert child before its separately owned group exists.
// It is not a Capture result and cannot be used as an active group identity.
type Preparation struct {
	PID         int    `json:"pid"`
	Start       uint64 `json:"start"`
	ParentGroup int    `json:"parent_group"`
}

func CapturePreparation(pid int, original Identity) (Preparation, error) {
	if pid <= 0 || original.Group <= 0 || original.Leader != original.Group || original.Start == 0 || original.Mark == "" || pid == original.Leader {
		return Preparation{}, ErrUnproven
	}
	parent, found, err := status(original.Leader)
	if err != nil {
		return Preparation{}, err
	}
	if !found || !parent.live || parent.start != original.Start || parent.group != original.Group {
		return Preparation{}, fmt.Errorf("%w: original leader no longer owns admission", ErrUnproven)
	}
	child, found, err := status(pid)
	if err != nil {
		return Preparation{}, err
	}
	if !found || !child.live || child.group != original.Group || child.start < original.Start {
		return Preparation{}, fmt.Errorf("%w: inert child differs from its original group", ErrUnproven)
	}
	return Preparation{PID: pid, Start: child.start, ParentGroup: original.Group}, nil
}

// CapturedGroup verifies the explicit split while the original child still
// pins its PID. A caller must durably record the returned identity before exec.
func (p Preparation) CapturedGroup(mark string) (Identity, error) {
	if p.PID <= 0 || p.Start == 0 || p.ParentGroup <= 0 || mark == "" {
		return Identity{}, ErrUnproven
	}
	id, err := Capture(p.PID, mark)
	if err != nil {
		return Identity{}, err
	}
	if id.Start != p.Start {
		return Identity{}, fmt.Errorf("%w: inert child identity changed before split", ErrUnproven)
	}
	return id, nil
}

// SettlePreparation confirms an inert transition only after its original
// Agent group has been settled. It never turns an intended split into a
// group identity: a running split requires a real Capture of the same child
// and start before it can be signalled.
//
// The helper cannot fork or execute before its active identity is committed.
// A missing child therefore requires positive evidence that its possible
// split group ended. PID reuse or an unexpected group stays unconfirmed.
func SettlePreparation(p Preparation, original Identity, mark string, ran, here Place, deadline time.Time) error {
	if p.PID <= 0 || p.PID == original.Leader || p.Start == 0 ||
		p.Start < original.Start || p.ParentGroup != original.Group ||
		original.Group <= 0 || original.Leader != original.Group ||
		original.Start == 0 || original.Mark == "" || mark == "" {
		return fmt.Errorf("%w: terminal preparation is incomplete", ErrUnproven)
	}
	if ran.Boot == "" || here.Boot == "" || ran.Machine != here.Machine {
		return fmt.Errorf("%w: terminal preparation place is unknown or differs", ErrUnproven)
	}
	if ran.Boot != here.Boot {
		if ran.Machine != "" {
			// This known machine rebooted; none of its old processes survives.
			return nil
		}
		return fmt.Errorf("%w: terminal preparation boot is not comparable", ErrUnproven)
	}
	if ran.Namespace != here.Namespace {
		return fmt.Errorf("%w: terminal preparation pid namespace differs", ErrUnproven)
	}
	for delay := time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		child, found, err := status(p.PID)
		if err != nil {
			return err
		}
		if found && child.start != p.Start {
			return fmt.Errorf("%w: terminal preparation pid was reused", ErrUnproven)
		}
		switch {
		case !found:
			remains, err := Inspect(p.PID)
			if err != nil {
				return err
			}
			if remains.Ended() {
				return nil
			}
			// No live child pins the intended group. Even a marked member
			// cannot retroactively supply the missing split Capture.
		case child.group == p.ParentGroup:
			if !child.live {
				// The exact unsplit child runs nothing, and the original
				// group was positively settled before this call.
				return nil
			}
			return fmt.Errorf("%w: original group still has its inert child", ErrUnproven)
		case child.group != p.PID:
			return fmt.Errorf("%w: terminal preparation moved to another group", ErrUnproven)
		default:
			remains, err := Inspect(p.PID)
			if err != nil {
				return err
			}
			if remains.Ended() {
				return nil
			}
			// The exact original child still pins its PID and now really leads
			// this group. Capture rechecks start; Settle judges identity again
			// before each signal. A later Setenv mark is not required here.
			id, err := p.CapturedGroup(mark)
			if err != nil {
				return err
			}
			if err := Settle(id, ran, here, time.Until(deadline)); err != nil {
				return err
			}
			after, found, err := status(p.PID)
			if err != nil {
				return err
			}
			if found && after.start != p.Start {
				return fmt.Errorf("%w: terminal preparation pid changed during cleanup", ErrUnproven)
			}
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("%w: terminal split is not positively identified or ended", ErrUnproven)
		}
		time.Sleep(min(delay, left))
	}
}
