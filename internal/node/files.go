package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/skills"
)

func (r *Registry) Files(ctx context.Context, name string, req nodewire.FileRequest) (string, error) {
	if name == "" {
		return runFileOperation(ctx, req)
	}
	var reply nodewire.FileReply
	if err := r.operation(ctx, name, nodewire.StreamFiles, nodewire.FeatureFiles, req, &reply); err != nil {
		return "", err
	}
	return reply.Data, artifact.DecodeFailure(reply.Error)
}

func (s *Server) runFiles(ctx context.Context, stream *nodewire.Stream) {
	defer stream.Close()
	ctx, cancel := operationContext(ctx, stream)
	defer cancel()
	var req nodewire.FileRequest
	var reply nodewire.FileReply
	if err := json.NewDecoder(io.LimitReader(stream, nodewire.MaxPayload)).Decode(&req); err != nil {
		reply.Error = &nodewire.OperationFailure{Code: "invalid_request", Message: err.Error()}
	} else {
		data, err := runFileOperation(ctx, req)
		reply.Data, reply.Error = data, artifact.EncodeFailure(err)
	}
	if err := json.NewEncoder(stream).Encode(reply); err != nil {
		log.Printf("steve-node: files reply: %v", err)
	}
}

func runFileOperation(ctx context.Context, req nodewire.FileRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if req.Op != nodewire.FileSearchPath && (!filepath.IsAbs(req.Path) || strings.ContainsRune(req.Path, 0)) {
		return "", &nodewire.OperationFailure{Code: "invalid_request", Message: "file operation requires an absolute path"}
	}
	switch req.Op {
	case nodewire.FileMkdir:
		return "", os.MkdirAll(req.Path, 0o700)
	case nodewire.FileClone:
		return "", cloneRepository(ctx, req.Path, req.Source)
	case nodewire.FileImportSkill:
		return skills.PackImport(ctx, req.Path)
	case nodewire.FileSearchPath:
		return os.Getenv("PATH"), nil
	default:
		return "", &nodewire.OperationFailure{Code: "invalid_request", Message: fmt.Sprintf("unknown file operation %q", req.Op)}
	}
}

// cloneRepository stages beside dest so a failed clone leaves no destination.
func cloneRepository(ctx context.Context, dest, source string) error {
	if source == "" {
		return &nodewire.OperationFailure{Code: "invalid_request", Message: "clone needs a source"}
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("clone destination %s: %w", dest, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".steve-clone-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--", source, filepath.Join(tmp, "repo"))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return &artifact.GitError{Command: "clone", Code: code, Stderr: strings.TrimSpace(string(out)) + ": " + err.Error()}
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("clone destination %s: %w", dest, os.ErrExist)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(filepath.Join(tmp, "repo"), dest)
}
