// Package datalevel is the data classification the hub assigns projects,
// nodes and itself. It depends on nothing else in Steve, so configuration,
// storage and transport can all compare levels without importing each other.
package datalevel

// Level is a data level. Levels order public < internal < restricted <
// sealed; an artifact's label is the maximum over its closure and a node may
// only hold what its own level admits.
type Level string

const (
	Public     Level = "public"
	Internal   Level = "internal"
	Restricted Level = "restricted"
	Sealed     Level = "sealed"
)

var order = map[Level]int{Public: 0, Internal: 1, Restricted: 2, Sealed: 3}

// Admits reports whether data at level l may sit at a place of level at.
func (l Level) Admits(at Level) bool { return order[at] >= order[l] }

// Valid says whether the level is one of the four.
func (l Level) Valid() bool { _, ok := order[l]; return ok }

// OrDefault is internal when unset.
func (l Level) OrDefault() Level {
	if l == "" {
		return Internal
	}
	return l
}
