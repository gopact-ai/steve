package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// ExitError is a command that ran and failed, with what it printed. It is
// distinct from not being able to run it at all: a verification that fails
// is information about the work, a node that cannot be reached is not.
type ExitError struct {
	Code   int
	Output string
}

// ExitCode lets callers recognize a script's status through wrapped errors,
// using the same interface as a local exec.ExitError.
func (e ExitError) ExitCode() int { return e.Code }

func (e ExitError) Error() string {
	out := strings.TrimSpace(e.Output)
	if len(out) > 400 {
		out = "…" + out[len(out)-400:]
	}
	if out == "" {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return fmt.Sprintf("exit %d: %s", e.Code, out)
}

// Exec runs a command on a node, in dir, and returns its combined output.
// An empty node means the hub itself. Verification uses this so a step's
// check runs where the step's work is.
func (r *Registry) Exec(ctx context.Context, nodeName, dir, command string) (string, error) {
	if nodeName == "" {
		return execLocal(ctx, dir, command)
	}
	c, err := r.connect(ctx, nodeName)
	if err != nil {
		return "", err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamExec, Command: command, Dir: dir})
	if err != nil {
		return "", fmt.Errorf("open exec stream on %q: %w", nodeName, err)
	}
	defer stream.Close()

	done := make(chan struct{})
	var output []byte
	var readErr error
	go func() {
		defer close(done)
		output, readErr = io.ReadAll(stream)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	text := string(output)
	// The exit status rides the close reason; EOF means the stream ended
	// without one, which is a node that died mid-command.
	if readErr == nil || errors.Is(readErr, io.EOF) {
		return text, fmt.Errorf("node %q closed the command stream without an exit status", nodeName)
	}
	reason := readErr.Error()
	if rest, ok := strings.CutPrefix(reason, nodewire.ExitPrefix); ok {
		code, convErr := strconv.Atoi(strings.TrimSpace(rest))
		if convErr != nil {
			return text, fmt.Errorf("node %q reported a malformed exit status %q", nodeName, reason)
		}
		if code != 0 {
			return text, ExitError{Code: code, Output: text}
		}
		return text, nil
	}
	return text, fmt.Errorf("node %q: %s", nodeName, reason)
}

func execLocal(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return string(out), ExitError{Code: exit.ExitCode(), Output: string(out)}
		}
		return string(out), err
	}
	return string(out), nil
}

// PutBlob sends a file to the node's blob directory under name.
func (r *Registry) PutBlob(ctx context.Context, nodeName, name string, content io.Reader, size int64) error {
	c, err := r.connect(ctx, nodeName)
	if err != nil {
		return err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamBlob, Command: "put " + name})
	if err != nil {
		return fmt.Errorf("open blob stream on %q: %w", nodeName, err)
	}
	defer stream.Close()
	if err := nodewire.WriteSize(stream, size); err != nil {
		return err
	}
	if _, err := io.Copy(stream, content); err != nil {
		return fmt.Errorf("send blob %s to %q: %w", name, nodeName, err)
	}
	return awaitExit(ctx, stream, nodeName)
}

// GetBlob fetches a file from the node's blob directory.
func (r *Registry) GetBlob(ctx context.Context, nodeName, name string, into io.Writer) error {
	c, err := r.connect(ctx, nodeName)
	if err != nil {
		return err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamBlob, Command: "get " + name})
	if err != nil {
		return fmt.Errorf("open blob stream on %q: %w", nodeName, err)
	}
	defer stream.Close()
	size, err := nodewire.ReadSize(stream)
	if err != nil {
		return fmt.Errorf("blob %s from %q: %w", name, nodeName, err)
	}
	if _, err := io.CopyN(into, stream, size); err != nil {
		return fmt.Errorf("receive blob %s from %q: %w", name, nodeName, err)
	}
	return awaitExit(ctx, stream, nodeName)
}

// awaitExit drains the stream until the node closes it with an exit status.
func awaitExit(ctx context.Context, stream *nodewire.Stream, nodeName string) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stream)
		done <- err
	}()
	var readErr error
	select {
	case readErr = <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		return fmt.Errorf("node %q closed the blob stream without an exit status", nodeName)
	}
	if rest, ok := strings.CutPrefix(readErr.Error(), nodewire.ExitPrefix); ok {
		if code, convErr := strconv.Atoi(strings.TrimSpace(rest)); convErr != nil || code != 0 {
			return fmt.Errorf("node %q blob transfer failed with status %s", nodeName, strings.TrimSpace(rest))
		}
		return nil
	}
	return readErr
}

// Git reports what the node advertised for holding workspaces: its git
// version (empty when absent), its workspace root and its state directory.
func (r *Registry) Git(ctx context.Context, nodeName string) (string, string, string, error) {
	advert, err := r.Advert(ctx, nodeName)
	if err != nil {
		return "", "", "", err
	}
	return advert.Git, advert.WorkspaceRoot, advert.StateDir, nil
}

// Level is the data level the hub assigned the node; the hub itself (node
// "") answers with what the caller configured for it via SetHubLevel.
func (r *Registry) Level(ctx context.Context, nodeName string) (string, error) {
	if nodeName == "" {
		return r.hubLevel(), nil
	}
	cfg, ok := r.config(nodeName)
	if !ok {
		return "", fmt.Errorf("node %q is not configured", nodeName)
	}
	if cfg.Level == "" {
		return "internal", nil
	}
	return cfg.Level, nil
}

// Region is the region the node's leases are issued in ("" is the hub's).
func (r *Registry) Region(_ context.Context, nodeName string) (string, error) {
	if nodeName == "" {
		return "", nil
	}
	cfg, ok := r.config(nodeName)
	if !ok {
		return "", fmt.Errorf("node %q is not configured", nodeName)
	}
	return cfg.Region, nil
}

// PeerAddr is the address other nodes reach the node at.
func (r *Registry) PeerAddr(nodeName string) (string, error) {
	cfg, ok := r.config(nodeName)
	if !ok {
		return "", fmt.Errorf("node %q is not configured", nodeName)
	}
	if cfg.PeerAddr != "" {
		return cfg.PeerAddr, nil
	}
	return cfg.Addr, nil
}

// Grant tells a node to admit one peer for one blob.
func (r *Registry) Grant(ctx context.Context, nodeName, token, name string, ttl time.Duration) error {
	c, err := r.connect(ctx, nodeName)
	if err != nil {
		return err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamGrant, Command: fmt.Sprintf("%s %s %d", token, name, int(ttl.Seconds()))})
	if err != nil {
		return fmt.Errorf("open grant stream on %q: %w", nodeName, err)
	}
	defer stream.Close()
	return awaitExit(ctx, stream, nodeName)
}

// Fetch tells a node to pull a blob from a peer that granted it.
func (r *Registry) Fetch(ctx context.Context, nodeName, peerAddr, token, name string) error {
	c, err := r.connect(ctx, nodeName)
	if err != nil {
		return err
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamFetch, Command: peerAddr + " " + token + " " + name})
	if err != nil {
		return fmt.Errorf("open fetch stream on %q: %w", nodeName, err)
	}
	defer stream.Close()
	return awaitExit(ctx, stream, nodeName)
}
