package procgroup

import (
	"bytes"
	"encoding/binary"
)

// procargsEnvironment finds the environment in what macOS's kern.procargs2
// returns for a process: the argument count, the executable's path, padding,
// the arguments, then the environment, every string NUL-terminated.
func procargsEnvironment(raw []byte) ([]byte, bool) {
	if len(raw) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(raw))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, false
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	for ; argc > 0; argc-- {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, false
		}
		rest = rest[end+1:]
	}
	return rest, true
}
