package gitrepo

import (
	"time"

	"github.com/gopact-ai/steve/internal/budget"
)

// ReviewLimits bound projections independently of artifact storage limits.
type ReviewLimits struct {
	MaxChanges   int
	MaxDiffBytes int
	MaxFileBytes int
	MaxEntries   int
	Timeout      time.Duration
}

// WithDefaults fills each unset budget with its default.
func (p ReviewLimits) WithDefaults() ReviewLimits {
	if p.MaxChanges <= 0 {
		p.MaxChanges = budget.ReviewChanges
	}
	if p.MaxDiffBytes <= 0 {
		p.MaxDiffBytes = budget.ReviewDiffBytes
	}
	if p.MaxFileBytes <= 0 {
		p.MaxFileBytes = budget.ReviewFileBytes
	}
	if p.MaxEntries <= 0 {
		p.MaxEntries = budget.ReviewEntries
	}
	if p.Timeout <= 0 {
		p.Timeout = budget.ReviewTimeout
	}
	return p
}
