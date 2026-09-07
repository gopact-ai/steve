package sshconnect

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Output struct {
	Stdout string
	Stderr string
}

type Runner interface {
	Run(context.Context, []string, string) (Output, error)
	Upload(context.Context, []string, io.Reader) (Output, error)
}

// OpenSSH uses the platform SSH client, retaining its authentication, config,
// and proxy support. The script is stdin; no local shell interprets arguments.
type OpenSSH struct{ Binary string }

func (s OpenSSH) Run(ctx context.Context, args []string, input string) (Output, error) {
	return s.Upload(ctx, args, strings.NewReader(input))
}

// Upload streams bytes directly to SSH stdin. Binary data never becomes a
// command argument, environment variable, shell fragment or log message.
func (s OpenSSH) Upload(ctx context.Context, args []string, input io.Reader) (Output, error) {
	binary := s.Binary
	if binary == "" {
		binary = "ssh"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = input
	stdout, stderr := &boundedOutput{}, &boundedOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return Output{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type boundedOutput struct{ bytes.Buffer }

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - w.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}
