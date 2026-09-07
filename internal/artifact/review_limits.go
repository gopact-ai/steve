package artifact

import "time"

// ReviewLimits bound projections independently of artifact storage limits.
type ReviewLimits struct {
	MaxChanges   int
	MaxDiffBytes int
	MaxFileBytes int
	MaxEntries   int
	Timeout      time.Duration
}

func (p ReviewLimits) defaults() ReviewLimits {
	if p.MaxChanges <= 0 {
		p.MaxChanges = MaxChanges
	}
	if p.MaxDiffBytes <= 0 {
		p.MaxDiffBytes = MaxDiffBytes
	}
	if p.MaxFileBytes <= 0 {
		p.MaxFileBytes = MaxFileBytes
	}
	if p.MaxEntries <= 0 {
		p.MaxEntries = MaxEntries
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultReviewTimeout
	}
	return p
}
