package sshconnect

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Connection is one authenticated SSH transport retained from plan checking
// through installation. Later config edits cannot redirect its channels.
type Connection interface {
	Run(context.Context, string, string) (Output, error)
	Upload(context.Context, string, io.Reader) (Output, error)
	Close() error
}

type ConnectionBinder interface {
	Bind(context.Context, string, []string) (Connection, error)
}

type sshConnection struct {
	runner      OpenSSH
	dir, socket string
	mu          sync.Mutex
	closed      bool
}

// Bind creates an isolated ControlMaster with no command or forwarded ports.
// ControlPersist bounds orphaned masters if the owning application exits.
func (r OpenSSH) Bind(ctx context.Context, alias string, arguments []string) (Connection, error) {
	dir, err := os.MkdirTemp("/tmp", "steve-ssh-")
	if err != nil {
		return nil, fail("ssh", "connection_setup", "无法创建私有 SSH 连接目录", "检查本机临时目录权限后重试")
	}
	connection := &sshConnection{runner: r, dir: dir, socket: filepath.Join(dir, "master")}
	keep := false
	defer func() {
		if !keep {
			_ = connection.Close()
		}
	}()
	args := []string{"-M", "-N", "-f", "-S", connection.socket, "-o", "ControlMaster=yes", "-o", "ControlPersist=300"}
	for i := 0; i < len(arguments); i++ {
		if arguments[i] == "--" {
			break
		}
		if arguments[i] == "-o" && i+1 < len(arguments) {
			key, _, _ := strings.Cut(arguments[i+1], "=")
			if key == "ControlMaster" || key == "ControlPath" || key == "ControlPersist" {
				i++
				continue
			}
		}
		args = append(args, arguments[i])
	}
	args = append(args, "--", alias)
	binary := r.Binary
	if binary == "" {
		binary = "ssh"
	}
	// File descriptors avoid inherited pipe readers surviving ssh's daemon
	// fork. Diagnostic data stays in the private directory and is classified.
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	diagnostic, err := os.OpenFile(filepath.Join(dir, "diagnostic"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer diagnostic.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, diagnostic
	if err := cmd.Run(); err != nil {
		_, _ = diagnostic.Seek(0, io.SeekStart)
		data, _ := io.ReadAll(io.LimitReader(diagnostic, 64<<10))
		return nil, connectionError(ctx, string(data))
	}
	if _, err := r.Run(ctx, connection.controlArguments("check"), ""); err != nil {
		return nil, fail("ssh", "connection_lost", "固定 SSH 连接未能建立", "重新检查机器并生成安装计划")
	}
	keep = true
	return connection, nil
}

func (c *sshConnection) arguments(command string) []string {
	// A missing multiplex socket otherwise falls back to a fresh SSH login.
	// The fixed failing proxy makes that fallback impossible even if this
	// socket disappears between the check and the channel request.
	return []string{"-T", "-F", os.DevNull, "-S", c.socket, "-o", "ControlMaster=no", "-o", "ProxyCommand=/usr/bin/false", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none", "-o", "ConnectTimeout=2", "--", "steve-bound", command}
}

func (c *sshConnection) controlArguments(action string) []string {
	return []string{"-F", os.DevNull, "-S", c.socket, "-O", action, "-o", "ProxyCommand=/usr/bin/false", "--", "steve-bound"}
}

func (c *sshConnection) available() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fail("ssh", "connection_lost", "原检查使用的 SSH 连接已关闭", "重新检查并审阅安装计划；不会改用其他连接发送凭据")
	}
	return nil
}

func (c *sshConnection) Run(ctx context.Context, command, input string) (Output, error) {
	if err := c.available(); err != nil {
		return Output{}, err
	}
	return c.runner.Run(ctx, c.arguments(command), input)
}

func (c *sshConnection) Upload(ctx context.Context, command string, input io.Reader) (Output, error) {
	if err := c.available(); err != nil {
		return Output{}, err
	}
	return c.runner.Upload(ctx, c.arguments(command), input)
}

func (c *sshConnection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.runner.Run(ctx, c.controlArguments("exit"), "")
	return os.RemoveAll(c.dir)
}
