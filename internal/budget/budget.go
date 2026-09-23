// Package budget holds the default budgets Steve's work runs under. The
// configuration offers them as its defaults and the packages that spend
// them fall back to the same values, so both import this package and
// neither imports the other.
package budget

import "time"

// Plan steps, verification and planning.
const (
	StepTimeout      = 15 * time.Minute
	VerifyTimeout    = 10 * time.Minute
	PlanningTimeout  = 3 * time.Minute
	PlanningAttempts = 2
)

// Snapshots: staging stops before git reads blobs past these.
const (
	SnapshotFiles     = 20_000
	SnapshotBytes     = 2 * 1024 * 1024 * 1024
	SnapshotFileBytes = 200 * 1024 * 1024
)

// Reviews: an index is at most ReviewChanges entries, a file's diff at most
// ReviewDiffBytes, a browsed file at most ReviewFileBytes, a directory
// listing at most ReviewEntries, and git gets ReviewTimeout.
const (
	ReviewChanges   = 500
	ReviewDiffBytes = 200 * 1024
	ReviewFileBytes = 200 * 1024
	ReviewEntries   = 2000
	ReviewTimeout   = 30 * time.Second
)
