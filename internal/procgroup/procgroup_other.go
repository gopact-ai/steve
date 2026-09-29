//go:build !linux && !darwin

package procgroup

// Here reports where this process runs.
func Here() (Place, error) { return Place{}, ErrUnsupported }

// Capture reads the identity of the group pid leads.
func Capture(int, string) (Identity, error) { return Identity{}, ErrUnsupported }

// WaitExit returns once the child pid has exited, leaving it unreaped.
func WaitExit(int) error { return ErrUnsupported }

// Live reports whether any process in the group still runs.
func Live(int) (bool, error) { return false, ErrUnsupported }

// Kill sends SIGKILL to every process in a group.
func Kill(int) error { return ErrUnsupported }

// Gone reports whether the kernel finds no process in a group.
func Gone(int) (bool, error) { return false, ErrUnsupported }

func status(int) (process, bool, error) { return process{}, false, ErrUnsupported }

func members(int) ([]process, error) { return nil, ErrUnsupported }

func carriesMark(int, string) bool { return false }
