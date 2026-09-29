//go:build !linux && !darwin

package procgroup

// Here reports where this process runs.
func Here() (Place, error) { return Place{}, ErrUnsupported }

// Capture reads the identity of the group pid leads.
func Capture(int, string) (Identity, error) { return Identity{}, ErrUnsupported }

// WaitExit returns once the child pid has exited, leaving it unreaped.
func WaitExit(int) error { return ErrUnsupported }

// Kill sends SIGKILL to every process in a group.
func Kill(int) error { return ErrUnsupported }

func ownGroup() int { return 0 }

func gone(int) (bool, error) { return false, ErrUnsupported }

func status(int) (process, bool, error) { return process{}, false, ErrUnsupported }

func members(int) (listing, error) { return listing{}, ErrUnsupported }

func carriesMark(int, string) bool { return false }
