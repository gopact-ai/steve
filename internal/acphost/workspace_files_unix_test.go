//go:build unix

package acphost

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
)

func TestWorkspaceFilesSpecialFilesAndAtomicMode(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	original := filepath.Join(root, "original.txt")
	if err := os.WriteFile(original, []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "shared.txt")
	if err := os.Link(original, shared); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Base(original), link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		// A FIFO with no writer must return a refusal, not block in Open.
		short, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if _, err := a.client.ReadTextFile(short, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: fifo}); err == nil {
			return nil, errors.New("FIFO was treated as text")
		}
		for _, path := range []string{fifo, shared, original, link} {
			if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "damage"}); err == nil {
				return nil, errors.New("shared, symlink or special file was overwritten")
			}
		}
		read, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: link})
		if err != nil || read.Content != "before" {
			return nil, errors.New("confined regular-file symlink read failed")
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "special-file safeguards", nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(original)
	if err != nil || string(body) != "before" {
		t.Fatal("refused write damaged the original hardlinked file")
	}
	if err := os.Remove(shared); err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: original, Content: "after"})
		if err != nil {
			return nil, err
		}
		_, err = a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: filepath.Join(root, "created", "nested", "note"), Content: "nested"})
		if err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "atomic text replacement", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(original)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("existing file mode lost: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".steve-text-") && entry.Name()[:len(".steve-text-")] == ".steve-text-" {
			t.Fatal("operation staging file leaked")
		}
	}
}

func TestWorkspaceFilesReadOnlyOSFileIsNotReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the file's ordinary write permission")
	}
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	path := filepath.Join(root, "readonly.txt")
	if err := os.WriteFile(path, []byte("original"), 0400); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "not permitted"})
		if err := expectFilesRefused(err); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "OS permissions", nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) || after.Mode() != info.Mode() {
		t.Fatal("read-only file identity or permission changed")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "original" {
		t.Fatal("read-only file bytes changed")
	}
}

type partialTextReader struct {
	data []byte
	err  error
}

func (r *partialTextReader) Read(dst []byte) (int, error) {
	n := copy(dst, r.data)
	r.data = r.data[n:]
	return n, r.err
}
func TestWorkspaceFilesPartialReadErrorIsNotAnEOF(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		want    string
		refused bool
	}{
		{"IO-error", syscall.EIO, "", true}, {"EOF-final-line", io.EOF, "partial", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReader(&partialTextReader{data: []byte("partial"), err: test.err})
			got, err := readWorkspaceText(reader, 1, filesNumber(1), func() error { return nil })
			if test.refused {
				if err == nil {
					t.Fatalf("partial read error reported successful content %q", got)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("final EOF line: %q %v", got, err)
			}
		})
	}
}
func TestWorkspaceFilesDoesNotCleanSymlinkParentTraversal(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	if err := os.MkdirAll(filepath.Join(root, "actual", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "actual", "target"), []byte("real target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("decoy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual/child", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	path := root + "/alias/../target"
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		read, err := a.client.ReadTextFile(ctx, &acp.ReadTextFileRequest{SessionID: req.SessionID, Path: path})
		if err != nil {
			return nil, err
		}
		if read.Content != "real target" {
			return nil, errors.New("callback silently changed symlink parent traversal target")
		}
		if _, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "updated"}); err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "native path semantics", nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, want string }{{filepath.Join(root, "target"), "decoy"}, {filepath.Join(root, "actual", "target"), "updated"}} {
		got, err := os.ReadFile(test.path)
		if err != nil || string(got) != test.want {
			t.Fatalf("native target %q: %q %v", test.path, got, err)
		}
	}
}
func TestWorkspaceFilesPreservesModeDespiteUmask(t *testing.T) {
	h, a, root, sid, generation := workspaceFilesHost(t, true, "auto")
	path := filepath.Join(root, "mode.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	a.prompt = func(ctx context.Context, req *acp.PromptRequest) (*acp.PromptResponse, error) {
		_, err := a.client.WriteTextFile(ctx, &acp.WriteTextFileRequest{SessionID: req.SessionID, Path: path, Content: "after"})
		if err != nil {
			return nil, err
		}
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	if _, _, err := h.Prompt(t.Context(), sid, generation, "file mode preservation", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("existing mode changed by process umask: %v", info.Mode())
	}
}
