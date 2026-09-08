package project

type CloneState string

const (
	CloneRunning     CloneState = "running"
	CloneSucceeded   CloneState = "succeeded"
	CloneFailed      CloneState = "failed"
	CloneUnconfirmed CloneState = "unconfirmed"
)

// BlocksWorkspace retains isolation until the clone's completion is confirmed.
func (s CloneState) BlocksWorkspace() bool {
	return s == CloneRunning || s == CloneUnconfirmed
}
