//go:build !unix

package pihome

// lockBase serializes within this process only: without flock, two Steve
// processes sharing one state directory are not excluded from each other.
func lockBase(base string) (func(), error) {
	mu := lockFor(base)
	mu.Lock()
	return mu.Unlock, nil
}
