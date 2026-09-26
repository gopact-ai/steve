package nodebootstrap

import (
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/gopact-ai/steve/internal/i18n"
)

type Binary struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// InspectBinary reads the executable format and checksum without executing it.
// Installers can reject a wrong-platform build before registering a machine.
func InspectBinary(text i18n.Catalog, path string) (Binary, error) {
	f, metadata, err := OpenBinary(text, path)
	if f != nil {
		_ = f.Close()
	}
	return metadata, err
}

// OpenBinary verifies one open file and rewinds it for streaming. The caller
// retains that descriptor, so replacing the path cannot swap the upload.
func OpenBinary(text i18n.Catalog, path string) (*os.File, Binary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryUnreadable))
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryNotFile))
	}
	out := Binary{Size: info.Size()}
	if object, err := elf.NewFile(f); err == nil {
		out.OS = "linux"
		switch object.Machine {
		case elf.EM_X86_64:
			out.Arch = "amd64"
		case elf.EM_AARCH64:
			out.Arch = "arm64"
		}
	} else if object, err := macho.NewFile(f); err == nil {
		out.OS = "darwin"
		switch object.Cpu {
		case macho.CpuAmd64:
			out.Arch = "amd64"
		case macho.CpuArm64:
			out.Arch = "arm64"
		}
	}
	if out.OS == "" || out.Arch == "" {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryUnsupported))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryUnverifiable))
	}
	sum := sha256.New()
	if n, err := io.Copy(sum, io.LimitReader(f, out.Size+1)); err != nil || n != out.Size {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryChanging))
	}
	out.SHA256 = hex.EncodeToString(sum.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, Binary{}, errors.New(text.T(i18n.NodeBinaryVerifiedUnreadable))
	}
	keep = true
	return f, out, nil
}
