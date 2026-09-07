package nodebootstrap

import (
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

type Binary struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// InspectBinary reads the executable format and checksum without executing it.
// Installers can reject a wrong-platform build before registering a machine.
func InspectBinary(path string) (Binary, error) {
	f, metadata, err := OpenBinary(path)
	if f != nil {
		_ = f.Close()
	}
	return metadata, err
}

// OpenBinary verifies one open file and rewinds it for streaming. The caller
// retains that descriptor, so replacing the path cannot swap the upload.
func OpenBinary(path string) (*os.File, Binary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Binary{}, fmt.Errorf("无法读取节点安装包")
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return nil, Binary{}, fmt.Errorf("节点安装包必须是不超过 512 MiB 的普通文件")
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
		return nil, Binary{}, fmt.Errorf("节点安装包需要受支持的 Linux 或 macOS 可执行文件")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, Binary{}, fmt.Errorf("无法校验节点安装包")
	}
	sum := sha256.New()
	if n, err := io.Copy(sum, io.LimitReader(f, out.Size+1)); err != nil || n != out.Size {
		return nil, Binary{}, fmt.Errorf("无法校验节点安装包或安装包正在改变")
	}
	out.SHA256 = hex.EncodeToString(sum.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, Binary{}, fmt.Errorf("无法读取已校验的节点安装包")
	}
	keep = true
	return f, out, nil
}
