package acphost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
)

const (
	maxWorkspaceTextBytes = 1 << 20
	maxWorkspaceScanBytes = 8 << 20
	maxWorkspaceFileCalls = 32
	workspaceFileDrain    = 2 * time.Second
)

// workspaceFiles is the immutable directory grant of a confirmed session.
// It is not an OS sandbox for the agent's own filesystem accesses.
type workspaceFiles struct {
	cwd      string
	path     string
	identity os.FileInfo
	root     *os.Root
	retired  bool // guarded by Host.mu
	writes   sync.Mutex
}

func prepareWorkspaceFiles(cwd string) (*workspaceFiles, error) {
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return nil, errors.New("workspace files require an absolute workspace")
	}
	path, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, errors.New("workspace files directory is unavailable")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.New("workspace files directory is unavailable")
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() {
		root.Close()
		return nil, errors.New("workspace files require a directory")
	}
	return &workspaceFiles{cwd: cwd, path: path, identity: info, root: root}, nil
}

func (w *workspaceFiles) matches(other *workspaceFiles) bool {
	return w != nil && other != nil && w.cwd == other.cwd &&
		w.path == other.path && os.SameFile(w.identity, other.identity)
}

func (w *workspaceFiles) relative(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", fileError(acp.ErrorCodeInvalidParams, "Text-file path must be absolute and bounded")
	}
	for _, base := range []string{w.cwd, w.path} {
		prefix := strings.TrimRight(base, string(filepath.Separator)) + string(filepath.Separator)
		if strings.HasPrefix(path, prefix) {
			rel := strings.TrimPrefix(path, prefix)
			if rel != "" {
				return rel, nil
			}
		}
	}
	return "", fileError(acp.ErrorCodeInvalidParams, "Text-file path is outside the session workspace")
}

// Only opted-in local clients implement these optional interfaces. A raw
// remote transport must not serve a remote agent from the hub's filesystem.
type workspaceFileHandler struct{ *clientHandler }

type workspaceFileCall struct {
	h          *Host
	col        *collector
	sid        acp.SessionID
	generation uint64
	workspace  *workspaceFiles
	ctx        context.Context
	rpc        context.Context
	cancel     context.CancelFunc
	stopRPC    func() bool
	id         string
}

func fileError(code acp.ErrorCode, message string) error {
	return &acp.Error{Code: code, Message: message}
}

func fileIOError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fileError(acp.ErrorCodeRequestCanceled, "Text-file request was cancelled")
	case os.IsNotExist(err):
		return fileError(acp.ErrorCodeResourceNotFound, "Text file is unavailable")
	default:
		// Never reflect file contents, outside paths or credential-bearing
		// OS error text back across the protocol.
		return fileError(acp.ErrorCodeInternalError, "Text-file operation failed")
	}
}

func (ch *workspaceFileHandler) beginFileCall(ctx context.Context, sid acp.SessionID) (*workspaceFileCall, error) {
	// Initialize is called while ensureStarted holds h.mu. Reject a reverse
	// request before acquiring that lock, rather than deadlocking the handshake.
	if !ch.ready.Load() {
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Text-file request has no active turn")
	}
	h := ch.h
	h.mu.Lock()
	defer h.mu.Unlock()
	state, col := h.sessions[sid], h.collectors[sid]
	if !h.alive || h.isClosed || h.generation != ch.generation ||
		h.active[sid] != ch.generation || state == nil || state.workspace == nil ||
		col == nil || col.generation != ch.generation || col.filesClosing || state.workspace.retired ||
		col.ctx == nil || col.ctx.Err() != nil || ctx.Err() != nil {
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Text-file request has no active turn")
	}
	if len(h.fileCalls) >= maxWorkspaceFileCalls {
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Too many active text-file requests")
	}
	request, cancel := context.WithCancel(col.ctx)
	call := &workspaceFileCall{
		h: h, col: col, sid: sid, generation: ch.generation,
		workspace: state.workspace, ctx: request, rpc: ctx, cancel: cancel,
	}
	call.stopRPC = context.AfterFunc(ctx, cancel)
	if h.fileCalls == nil {
		h.fileCalls = map[*workspaceFileCall]struct{}{}
	}
	h.fileCalls[call] = struct{}{}
	h.nextFileCall++
	call.id = "workspace-file-" + strconv.FormatUint(ch.generation, 10) + "-" + strconv.FormatUint(h.nextFileCall, 10)
	if col.fileCount == 0 {
		col.filesDone = make(chan struct{})
	}
	col.fileCount++
	return call, nil
}

func (call *workspaceFileCall) valid() error {
	if err := call.rpc.Err(); err != nil {
		return fileIOError(err)
	}
	if err := call.ctx.Err(); err != nil {
		return fileIOError(err)
	}
	h := call.h
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.sessions[call.sid]
	if !h.alive || h.isClosed || h.generation != call.generation ||
		h.collectors[call.sid] != call.col || h.active[call.sid] != call.generation ||
		call.col.filesClosing || state == nil || state.workspace != call.workspace || call.workspace.retired {
		return fileError(acp.ErrorCodeInvalidRequest, "Text-file turn is no longer active")
	}
	return nil
}

func (call *workspaceFileCall) finish() {
	call.stopRPC()
	call.cancel()
	h := call.h
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.fileCalls, call)
	call.col.fileCount--
	if call.col.fileCount == 0 {
		close(call.col.filesDone)
		if call.col.filesClosing {
			h.forgetCollectorLocked(call.sid, call.col)
		}
	}
	h.closeUnusedWorkspaceLocked(call.workspace)
}

func (h *Host) closeUnusedWorkspaceLocked(workspace *workspaceFiles) {
	if workspace == nil || !workspace.retired {
		return
	}
	for call := range h.fileCalls {
		if call.workspace == workspace {
			return
		}
	}
	workspace.root.Close()
	delete(h.fileRoots, workspace)
}

func (h *Host) retireWorkspaceLocked(workspace *workspaceFiles) {
	if workspace != nil {
		workspace.retired = true
		h.closeUnusedWorkspaceLocked(workspace)
	}
}

func (h *Host) retireFileRootsLocked(generation uint64) {
	for workspace, owner := range h.fileRoots {
		if generation == 0 || owner == generation {
			h.retireWorkspaceLocked(workspace)
		}
	}
}

func (h *Host) forgetCollectorLocked(sid acp.SessionID, col *collector) {
	if h.collectors[sid] == col {
		delete(h.collectors, sid)
		if h.active[sid] == col.generation {
			delete(h.active, sid)
		}
	}
}

func (h *Host) retireCollector(sid acp.SessionID, col *collector) error {
	h.mu.Lock()
	col.filesClosing = true
	col.cancelAsk()
	done := col.filesDone
	if col.fileCount == 0 {
		h.forgetCollectorLocked(sid, col)
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	timer := time.NewTimer(workspaceFileDrain)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errors.New("accepted text-file requests have not settled")
	}
}

func (h *Host) cancelFileCallsLocked(generation uint64) {
	for call := range h.fileCalls {
		if generation == 0 || call.generation == generation {
			call.cancel()
		}
	}
}

func (call *workspaceFileCall) authorize(kind acp.ToolKind, rel string) error {
	if err := call.valid(); err != nil {
		return err
	}
	broker := call.h.cfg.Permission
	if broker.NeedsAsk(kind) {
		if call.col.ask == nil {
			return fileError(acp.ErrorCodeInvalidRequest, "Text-file permission was not granted")
		}
		options := []acp.PermissionOption{
			{OptionID: "workspace-file-allow", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: "workspace-file-reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		}
		outcome, err := call.col.ask(call.ctx, permission.Ask{
			SessionID: string(call.sid), Generation: call.generation,
			ToolCallID: call.id, ToolName: "Write workspace text file",
			Kind: kind, Reason: "Write `" + rel + "` in the session workspace.", Options: options,
		})
		if err != nil || outcome.Outcome != acp.RequestPermissionOutcomeTypeSelected ||
			outcome.OptionID != options[0].OptionID {
			return fileError(acp.ErrorCodeInvalidRequest, "Text-file permission was not granted")
		}
	} else if !broker.Allow(kind) {
		return fileError(acp.ErrorCodeInvalidRequest, "Text-file permission was not granted")
	}
	// An answer belongs to the captured collector, not a subsequent prompt
	// which happens to reuse the same SID and native generation.
	return call.valid()
}

func (call *workspaceFileCall) openRoot() (*os.Root, error) {
	if err := call.valid(); err != nil {
		return nil, err
	}
	current, err := os.Stat(call.workspace.cwd)
	if err != nil || !os.SameFile(current, call.workspace.identity) {
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Original workspace directory changed")
	}
	root, err := call.workspace.root.OpenRoot(".")
	if err != nil {
		return nil, fileIOError(err)
	}
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(info, call.workspace.identity) {
		root.Close()
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Original workspace directory changed")
	}
	return root, nil
}

func (ch *workspaceFileHandler) ReadTextFile(ctx context.Context, req *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
	if req == nil || (req.Line != nil && *req.Line == 0) {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file line must be one-based")
	}
	call, err := ch.beginFileCall(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	rel, err := call.workspace.relative(req.Path)
	if err != nil {
		return nil, err
	}
	if err := call.authorize(acp.ToolKindRead, rel); err != nil {
		return nil, err
	}
	root, err := call.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := openWorkspaceText(root, rel)
	if err != nil {
		return nil, fileIOError(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file request requires a regular file")
	}
	if req.Limit != nil && *req.Limit == 0 {
		return &acp.ReadTextFileResponse{Content: ""}, nil
	}
	start := uint32(1)
	if req.Line != nil {
		start = *req.Line
	}
	contentText, err := readWorkspaceText(bufio.NewReaderSize(file, 4096), start, req.Limit, call.valid)
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(contentText) {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file content must be UTF-8")
	}
	if err := call.valid(); err != nil {
		return nil, err
	}
	response := &acp.ReadTextFileResponse{Content: contentText}
	wire, err := json.Marshal(response)
	if err != nil || len(wire) > maxWorkspaceTextBytes-1024 {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file encoded response exceeds its byte limit")
	}
	return response, nil
}

func readWorkspaceText(input *bufio.Reader, start uint32, limit *uint32, check func() error) (string, error) {
	var content bytes.Buffer
	reader := input
	line, visited, selected := uint64(1), 0, uint64(0)
	for {
		if err := check(); err != nil {
			return "", err
		}
		part, readErr := reader.ReadSlice('\n')
		if readErr != nil && readErr != io.EOF && readErr != bufio.ErrBufferFull {
			return "", fileIOError(readErr)
		}
		visited += len(part)
		if visited > maxWorkspaceScanBytes {
			return "", fileError(acp.ErrorCodeInvalidParams, "Text-file scan exceeds its byte limit")
		}
		if line >= uint64(start) {
			if content.Len()+len(part) > maxWorkspaceTextBytes {
				return "", fileError(acp.ErrorCodeInvalidParams, "Text-file response exceeds its byte limit")
			}
			content.Write(part)
		}
		if readErr != bufio.ErrBufferFull {
			if line >= uint64(start) && len(part) > 0 {
				selected++
			}
			line++
			if limit != nil && selected >= uint64(*limit) {
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return "", fileIOError(readErr)
		}
	}
	return content.String(), nil
}

func (ch *workspaceFileHandler) WriteTextFile(ctx context.Context, req *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
	if req == nil || len(req.Content) > maxWorkspaceTextBytes || !utf8.ValidString(req.Content) {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file content must be bounded UTF-8")
	}
	call, err := ch.beginFileCall(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	rel, err := call.workspace.relative(req.Path)
	if err != nil {
		return nil, err
	}
	if err := call.authorize(acp.ToolKindEdit, rel); err != nil {
		return nil, err
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !call.workspace.writes.TryLock() {
		select {
		case <-call.ctx.Done():
			return nil, fileIOError(call.ctx.Err())
		case <-tick.C:
		}
	}
	defer call.workspace.writes.Unlock()
	root, err := call.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	mode := os.FileMode(0600)
	if info, err := root.Lstat(rel); err == nil {
		if !info.Mode().IsRegular() || !workspaceSingleLink(info) {
			return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file write requires an unshared regular file")
		}
		if err := checkWorkspaceTextWritable(root, rel); err != nil {
			return nil, fileIOError(err)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return nil, fileIOError(err)
	}
	if err := call.valid(); err != nil {
		return nil, err
	}
	parent := "."
	if index := strings.LastIndex(rel, string(filepath.Separator)); index >= 0 {
		parent = rel[:index]
	}
	if err := root.MkdirAll(parent, 0700); err != nil {
		return nil, fileIOError(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fileIOError(err)
	}
	temp := parent + string(filepath.Separator) + ".steve-text-" + hex.EncodeToString(nonce[:])
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return nil, fileIOError(err)
	}
	defer root.Remove(temp)
	// Creation applies the process umask; explicitly preserve the original mode.
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return nil, fileIOError(err)
	}
	_, writeErr := io.WriteString(file, req.Content)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return nil, fileIOError(err)
	}
	if err := call.valid(); err != nil {
		return nil, err
	}
	if info, err := root.Lstat(rel); err == nil {
		if !info.Mode().IsRegular() || !workspaceSingleLink(info) {
			return nil, fileError(acp.ErrorCodeInvalidParams, "Text-file write destination changed")
		}
	} else if !os.IsNotExist(err) {
		return nil, fileIOError(err)
	}
	if err := root.Rename(temp, rel); err != nil {
		return nil, fileIOError(err)
	}
	return &acp.WriteTextFileResponse{}, nil
}
