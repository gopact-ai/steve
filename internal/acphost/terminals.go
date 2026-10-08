package acphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/procgroup"
)

const (
	maxTerminals               = 32
	maxTerminalOutputBytes     = 1 << 20
	defaultTerminalOutputBytes = maxTerminalOutputBytes / maxTerminals
	terminalCleanupWait        = 5 * time.Second
)

// terminalRuntime owns one direct child, its separately admitted group and
// its output drain. Done does not mean only the leader was reaped.
type terminalRuntime interface {
	Preparation() procgroup.Preparation
	Mark() string
	Place() procgroup.Place
	Split(context.Context) (procgroup.Identity, error)
	Execute(context.Context) error
	PayloadAdmitted() bool
	Stop(context.Context) error
	NativeStopped() bool
	Done() <-chan struct{}
	ExitStatus() *acp.TerminalExitStatus
}

type hostTerminal struct {
	intent    TerminalIntent
	owner     TerminalOwner
	col       *collector
	workspace *workspaceFiles
	original  Process
	output    *terminalOutput
	limit     int
	start     context.Context
	cancel    context.CancelFunc
	setupDone chan struct{}
	// phase excludes payload consumption and cleanup admission, never an Owner
	// callback. Consume can run while the node's owner mutex is held.
	phase    sync.Mutex
	runtime  terminalRuntime
	closing  bool
	reserved bool
	admitted bool // EXEC gate consumed, not a claim of payload execution
	// cleanup serializes the monotonic durable stop callback and its retries.
	cleanup   sync.Mutex
	confirmed bool // guarded by Host.mu; set only after successful Stopped
	released  bool // guarded by Host.mu
}

type terminalHandler struct{ *clientHandler }
type workspaceTerminalHandler struct{ *terminalHandler }

func (ch *workspaceTerminalHandler) ReadTextFile(ctx context.Context, req *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
	return (&workspaceFileHandler{ch.clientHandler}).ReadTextFile(ctx, req)
}
func (ch *workspaceTerminalHandler) WriteTextFile(ctx context.Context, req *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
	return (&workspaceFileHandler{ch.clientHandler}).WriteTextFile(ctx, req)
}

var _ acp.TerminalHandler = (*terminalHandler)(nil)
var _ acp.TerminalHandler = (*workspaceTerminalHandler)(nil)

func (h *Host) terminalEnabled(proc Process) bool {
	return h.cfg.TerminalOwner != nil && TerminalsSupported && localTerminalAvailable(proc)
}

func terminalError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fileError(acp.ErrorCodeRequestCanceled, "Terminal request was cancelled")
	}
	return fileError(acp.ErrorCodeInternalError, "Terminal operation could not be confirmed")
}

func newTerminalID() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "nt_" + hex.EncodeToString(bytes[:]), nil
}

func terminalRequest(req *acp.CreateTerminalRequest) (int, error) {
	if req == nil || req.Command == "" || len(req.Command) > 4096 || strings.ContainsRune(req.Command, 0) || len(req.Args) > 128 || len(req.Env) > 128 {
		return 0, fileError(acp.ErrorCodeInvalidParams, "Terminal command must be bounded")
	}
	for _, arg := range req.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return 0, fileError(acp.ErrorCodeInvalidParams, "Terminal argument is invalid")
		}
	}
	seen := map[string]bool{}
	for _, env := range req.Env {
		if env.Name == "" || len(env.Name) > 128 || strings.ContainsAny(env.Name, "=\x00") || len(env.Value) > 8192 || strings.ContainsRune(env.Value, 0) || env.Name == procgroup.MarkVariable || seen[env.Name] {
			return 0, fileError(acp.ErrorCodeInvalidParams, "Terminal environment override is invalid")
		}
		seen[env.Name] = true
	}
	limit := defaultTerminalOutputBytes
	if req.OutputByteLimit != nil {
		if *req.OutputByteLimit > maxTerminalOutputBytes {
			return 0, fileError(acp.ErrorCodeInvalidParams, "Terminal output limit exceeds its byte budget")
		}
		limit = int(*req.OutputByteLimit)
	}
	return limit, nil
}

func terminalCwd(workspace *workspaceFiles, cwd *string) (string, error) {
	if cwd == nil || *cwd == "" {
		return ".", nil
	}
	if len(*cwd) > 4096 || strings.ContainsRune(*cwd, 0) {
		return "", fileError(acp.ErrorCodeInvalidParams, "Terminal cwd must be bounded")
	}
	if !filepath.IsAbs(*cwd) {
		return *cwd, nil
	} // Root interprets raw symlink/.. semantics.
	if strings.TrimRight(*cwd, string(filepath.Separator)) == strings.TrimRight(workspace.cwd, string(filepath.Separator)) || strings.TrimRight(*cwd, string(filepath.Separator)) == strings.TrimRight(workspace.path, string(filepath.Separator)) {
		return ".", nil
	}
	return workspace.relative(*cwd)
}

func (ch *terminalHandler) beginTerminal(ctx context.Context, sid acp.SessionID, class string) (*workspaceFileCall, error) {
	return (&workspaceFileHandler{ch.clientHandler}).beginWorkspaceCall(ctx, sid, class)
}

func (call *workspaceFileCall) authorizeTerminal(req *acp.CreateTerminalRequest) error {
	if err := call.valid(); err != nil {
		return err
	}
	broker := call.h.cfg.Permission
	if broker.NeedsAsk(acp.ToolKindExecute) {
		if call.col.ask == nil {
			return fileError(acp.ErrorCodeInvalidRequest, "Terminal execution permission was not granted")
		}
		options := []acp.PermissionOption{
			{OptionID: "terminal-allow", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionID: "terminal-reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		}
		argv := make([]any, 0, len(req.Args)+1)
		argv = append(argv, req.Command)
		for _, arg := range req.Args {
			argv = append(argv, arg)
		}
		raw := map[string]any{"command": argv}
		if req.Cwd != nil {
			raw["cwd"] = *req.Cwd
		}
		outcome, err := call.col.ask(call.ctx, permission.Ask{
			SessionID: string(call.sid), Generation: call.generation, ToolCallID: call.id,
			ToolName: "Run workspace terminal", Kind: acp.ToolKindExecute,
			Reason: permissionReason(acp.ToolCallUpdate{RawInput: raw}), Options: options,
		})
		if err != nil || outcome.Outcome != acp.RequestPermissionOutcomeTypeSelected || outcome.OptionID != options[0].OptionID {
			return fileError(acp.ErrorCodeInvalidRequest, "Terminal execution permission was not granted")
		}
	} else if !broker.Allow(acp.ToolKindExecute) {
		return fileError(acp.ErrorCodeInvalidRequest, "Terminal execution permission was not granted")
	}
	return call.valid()
}

func (t *hostTerminal) validate(call *workspaceFileCall) error {
	if err := call.valid(); err != nil {
		return err
	}
	if err := t.start.Err(); err != nil {
		return err
	}
	h := call.h
	h.mu.Lock()
	same := h.proc == t.original && h.processes[t.intent.Generation] == t.original && h.cfg.TerminalOwner != nil && !t.released
	h.mu.Unlock()
	if !same {
		return fileError(acp.ErrorCodeInvalidRequest, "Original terminal process changed")
	}
	current, err := os.Stat(t.workspace.cwd)
	if err != nil || !os.SameFile(current, t.workspace.identity) {
		return fileError(acp.ErrorCodeInvalidRequest, "Original workspace directory changed")
	}
	info, err := t.workspace.root.Stat(".")
	if err != nil || !os.SameFile(info, t.workspace.identity) {
		return fileError(acp.ErrorCodeInvalidRequest, "Original workspace directory changed")
	}
	return validateLocalTerminal(t.original)
}

func (ch *terminalHandler) CreateTerminal(ctx context.Context, req *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
	limit, err := terminalRequest(req)
	if err != nil {
		return nil, err
	}
	captured := *req
	captured.Args = append([]string(nil), req.Args...)
	captured.Env = append([]acp.EnvVariable(nil), req.Env...)
	if req.Cwd != nil {
		cwd := *req.Cwd
		captured.Cwd = &cwd
	}
	if req.OutputByteLimit != nil {
		bytes := *req.OutputByteLimit
		captured.OutputByteLimit = &bytes
	}
	req = &captured
	call, err := ch.beginTerminal(ctx, req.SessionID, "terminal-create")
	if err != nil {
		return nil, err
	}
	defer call.finish()
	if err := call.authorizeTerminal(req); err != nil {
		return nil, err
	}
	rel, err := terminalCwd(call.workspace, req.Cwd)
	if err != nil {
		return nil, err
	}
	root, err := call.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	cwd, err := openTerminalDirectory(root, rel)
	if err != nil {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal cwd is outside or unavailable in the workspace")
	}
	defer cwd.Close()
	if info, err := cwd.Stat(); err != nil || !info.IsDir() {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal cwd must be a directory")
	}
	id, err := newTerminalID()
	if err != nil {
		return nil, terminalError(err)
	}
	h := ch.h
	// Snapshot's collector -> Host order also serializes capacity reservation
	// with frozen swaps and reclamation of replaced references.
	call.col.mu.Lock()
	h.mu.Lock()
	original := h.proc
	owner := h.cfg.TerminalOwner
	if !h.terminalEnabled(original) {
		h.mu.Unlock()
		call.col.mu.Unlock()
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Owned terminal execution is unavailable")
	}
	count := len(call.col.terminalReleased)
	for _, previous := range h.terminals {
		if previous.col == call.col {
			count++
		}
	}
	if count >= maxTerminals {
		h.mu.Unlock()
		call.col.mu.Unlock()
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Terminal slots exhausted")
	}
	if h.terminalOutputBytes == nil {
		h.terminalOutputBytes = map[*collector]int{}
	}
	limit = min(limit, maxTerminalOutputBytes-h.terminalOutputBytes[call.col])
	h.terminalOutputBytes[call.col] += limit
	start, cancel := context.WithCancel(call.ctx)
	t := &hostTerminal{
		intent: TerminalIntent{ID: id, SessionID: req.SessionID, Generation: ch.generation},
		owner:  owner, col: call.col, workspace: call.workspace, original: original,
		output: newTerminalOutput(limit), limit: limit, start: start, cancel: cancel, setupDone: make(chan struct{}),
	}
	if h.terminals == nil {
		h.terminals = map[string]*hostTerminal{}
	}
	h.terminals[t.intent.ID] = t
	helperArgs := append([]string(nil), h.terminalHelperArgs...)
	h.mu.Unlock()
	call.col.mu.Unlock()
	defer func() {
		close(t.setupDone)
		t.phase.Lock()
		admitted := t.admitted
		t.phase.Unlock()
		if !admitted {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), terminalCleanupWait)
			defer cancel()
			if t.stop(cleanupCtx, h) == nil {
				h.releaseTerminal(t)
			}
		}
	}()
	config, err := localTerminalConfig(original, req)
	if err != nil {
		return nil, terminalError(err)
	}
	// Validate wire size before durable reservation or starting the inert child.
	if raw, err := json.Marshal(config); err != nil || len(raw) > 16<<10-1 {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal launch data exceeds its bound")
	}
	if err := t.validate(call); err != nil {
		return nil, terminalError(err)
	}
	if err := owner.Reserve(start, t.intent); err != nil {
		return nil, terminalError(err)
	}
	t.phase.Lock()
	t.reserved = true
	t.phase.Unlock()
	if err := t.validate(call); err != nil {
		return nil, terminalError(err)
	}
	t.phase.Lock()
	if t.closing {
		t.phase.Unlock()
		return nil, terminalError(context.Canceled)
	}
	runtime, prepareErr := prepareLocalTerminal(start, original, cwd, config, t.output, helperArgs)
	t.runtime = runtime // Retain even a failed started helper until positive proof.
	t.phase.Unlock()
	if prepareErr != nil {
		return nil, terminalError(prepareErr)
	}
	go h.watchTerminalOutput(t, runtime)
	if err := t.validate(call); err != nil {
		return nil, terminalError(err)
	}
	if err := owner.Prepared(start, t.intent, runtime.Preparation(), runtime.Mark()); err != nil {
		return nil, terminalError(err)
	}
	t.phase.Lock()
	var identity procgroup.Identity
	if err = t.validate(call); err == nil && !t.closing {
		identity, err = runtime.Split(start)
	} else if err == nil {
		err = context.Canceled
	}
	t.phase.Unlock()
	if err != nil {
		return nil, terminalError(err)
	}
	if err := t.validate(call); err != nil {
		return nil, terminalError(err)
	}
	if err := owner.Active(start, t.intent, identity, runtime.Place()); err != nil {
		return nil, terminalError(err)
	}
	validate := func() error { return t.validate(call) }
	consume := func(grant context.Context) error {
		// No Owner call under phase: the caller may hold Node's owner mutex.
		t.phase.Lock()
		defer t.phase.Unlock()
		if t.closing || t.admitted {
			return errors.New("terminal payload gate is unavailable")
		}
		if err := grant.Err(); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return err
		}
		err := runtime.Execute(grant)
		// Retain a successfully written EXEC even if cancellation or a lost
		// acknowledgement makes the caller's create result uncertain.
		t.admitted = runtime.PayloadAdmitted()
		return err
	}
	if err := validate(); err != nil {
		return nil, terminalError(err)
	}
	if err := owner.Admit(start, t.intent, consume, validate); err != nil {
		return nil, terminalError(err)
	}
	t.phase.Lock()
	admitted := t.admitted
	t.phase.Unlock()
	if !admitted {
		return nil, fileError(acp.ErrorCodeInvalidRequest, "Terminal payload was not admitted")
	}
	// Do not revalidate a cancelled create RPC here: after gate consumption,
	// its cancellation is not authority to kill the successful terminal.
	return &acp.CreateTerminalResponse{TerminalID: acp.TerminalID(t.intent.ID)}, nil
}

func (ch *terminalHandler) lookupTerminal(ctx context.Context, sid acp.SessionID, id acp.TerminalID, class string) (*workspaceFileCall, *hostTerminal, error) {
	call, err := ch.beginTerminal(ctx, sid, class)
	if err != nil {
		return nil, nil, err
	}
	ch.h.mu.Lock()
	t := ch.h.terminals[string(id)]
	valid := t != nil && !t.released && t.intent.SessionID == sid && t.intent.Generation == ch.generation && t.col == call.col && t.workspace == call.workspace
	ch.h.mu.Unlock()
	if valid {
		t.phase.Lock()
		valid = t.admitted
		t.phase.Unlock()
	}
	if !valid {
		call.finish()
		return nil, nil, fileError(acp.ErrorCodeResourceNotFound, "Terminal is not available in this turn")
	}
	return call, t, nil
}

func (t *hostTerminal) snapshot() (string, bool, *acp.TerminalExitStatus) {
	text, truncated := t.output.Snapshot()
	t.phase.Lock()
	runtime := t.runtime
	t.phase.Unlock()
	if runtime == nil {
		return text, truncated, nil
	}
	return text, truncated, runtime.ExitStatus()
}

func (ch *terminalHandler) TerminalOutput(ctx context.Context, req *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
	if req == nil {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal request is required")
	}
	call, t, err := ch.lookupTerminal(ctx, req.SessionID, req.TerminalID, "terminal-output")
	if err != nil {
		return nil, err
	}
	defer call.finish()
	text, truncated, status := t.snapshot()
	text, clipped := terminalWireText(text, status)
	truncated = truncated || clipped
	if err := call.valid(); err != nil {
		return nil, err
	}
	return &acp.TerminalOutputResponse{Output: text, Truncated: truncated, ExitStatus: status}, nil
}

// ACP frames have their own encoded-byte budget. Control bytes may use six
// JSON bytes per output byte; scan once and retain a fitting UTF-8 suffix.
func terminalWireText(text string, status *acp.TerminalExitStatus) (string, bool) {
	overhead, err := json.Marshal(acp.TerminalOutputResponse{Output: "", Truncated: false, ExitStatus: status})
	if err != nil {
		return "", true
	}
	remaining := maxTerminalOutputBytes - 1024 - len(overhead)
	start := len(text)
	for start > 0 {
		rune, bytes := utf8.DecodeLastRuneInString(text[:start])
		encoded := bytes
		switch rune {
		case '\b', '\t', '\n', '\f', '\r', '"', '\\':
			encoded = 2
		case '<', '>', '&', '\u2028', '\u2029':
			encoded = 6
		default:
			if rune < 0x20 {
				encoded = 6
			}
		}
		if encoded > remaining {
			break
		}
		remaining -= encoded
		start -= bytes
	}
	if start == 0 {
		return text, false
	}
	return strings.Clone(text[start:]), true
}

func (ch *terminalHandler) WaitForTerminalExit(ctx context.Context, req *acp.WaitForTerminalExitRequest) (*acp.WaitForTerminalExitResponse, error) {
	if req == nil {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal request is required")
	}
	call, t, err := ch.lookupTerminal(ctx, req.SessionID, req.TerminalID, "terminal-wait")
	if err != nil {
		return nil, err
	}
	defer call.finish()
	t.phase.Lock()
	runtime := t.runtime
	t.phase.Unlock()
	select {
	case <-call.ctx.Done():
		return nil, terminalError(call.ctx.Err()) // Cancel this waiter only.
	case <-runtime.Done():
	}
	if !runtime.NativeStopped() {
		return nil, terminalError(procgroup.ErrUnproven)
	}
	if err := t.stop(call.ctx, ch.h); err != nil {
		return nil, terminalError(err)
	}
	if err := call.valid(); err != nil {
		return nil, err
	}
	status := runtime.ExitStatus()
	if status == nil {
		return nil, terminalError(procgroup.ErrUnproven)
	}
	return &acp.WaitForTerminalExitResponse{ExitCode: status.ExitCode, Signal: status.Signal}, nil
}

func (t *hostTerminal) stop(ctx context.Context, h *Host) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !t.cleanup.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	defer t.cleanup.Unlock()
	h.mu.Lock()
	confirmed := t.confirmed
	h.mu.Unlock()
	if confirmed {
		return nil
	}
	t.cancel()
	select {
	case <-t.setupDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	t.phase.Lock()
	t.closing = true
	runtime, reserved := t.runtime, t.reserved
	t.phase.Unlock()
	if runtime != nil {
		if err := runtime.Stop(ctx); err != nil {
			return err
		}
		if !runtime.NativeStopped() {
			return procgroup.ErrUnproven
		}
	}
	// No phase / Host lock is held across a Node owner callback.
	if reserved {
		if err := t.owner.Stopped(ctx, t.intent); err != nil {
			return err
		}
	}
	h.mu.Lock()
	t.confirmed = true
	h.mu.Unlock()
	return nil
}

func (ch *terminalHandler) KillTerminal(ctx context.Context, req *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
	if req == nil {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal request is required")
	}
	call, t, err := ch.lookupTerminal(ctx, req.SessionID, req.TerminalID, "terminal-control")
	if err != nil {
		return nil, err
	}
	defer call.finish()
	cleanup, cancel := context.WithTimeout(call.ctx, terminalCleanupWait)
	defer cancel()
	if err := t.stop(cleanup, ch.h); err != nil {
		return nil, terminalError(err)
	}
	return &acp.KillTerminalResponse{}, nil
}

func (ch *terminalHandler) ReleaseTerminal(ctx context.Context, req *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
	if req == nil {
		return nil, fileError(acp.ErrorCodeInvalidParams, "Terminal request is required")
	}
	call, t, err := ch.lookupTerminal(ctx, req.SessionID, req.TerminalID, "terminal-control")
	if err != nil {
		return nil, err
	}
	defer call.finish()
	cleanup, cancel := context.WithTimeout(call.ctx, terminalCleanupWait)
	defer cancel()
	if err := t.stop(cleanup, ch.h); err != nil {
		return nil, terminalError(err)
	}
	ch.h.releaseTerminal(t)
	return &acp.ReleaseTerminalResponse{}, nil
}

func (h *Host) releaseTerminal(t *hostTerminal) {
	h.mu.Lock()
	if !t.confirmed || t.released {
		h.mu.Unlock()
		return
	}
	t.released = true
	h.mu.Unlock()
	text, _, _ := t.snapshot()
	t.col.mu.Lock()
	frozen := t.col.freezeTerminalLocked(t, text)
	h.mu.Lock()
	delete(h.terminals, t.intent.ID)
	h.terminalOutputBytes[t.col] += frozen - t.limit
	if h.terminalOutputBytes[t.col] == 0 {
		delete(h.terminalOutputBytes, t.col)
	}
	if t.col.filesClosing && t.col.fileCount == 0 {
		h.forgetCollectorLocked(t.intent.SessionID, t.col)
		retained := false
		for _, other := range h.terminals {
			retained = retained || other.col == t.col
		}
		if !retained {
			delete(h.terminalOutputBytes, t.col)
		}
	}
	h.closeUnusedWorkspaceLocked(t.workspace)
	h.mu.Unlock()
	t.col.mu.Unlock()
	t.col.publishTerminalProgress()
}

func (h *Host) terminalsConfirmedLocked(generation uint64) bool {
	for _, t := range h.terminals {
		if (generation == 0 || t.intent.Generation == generation) && !t.confirmed {
			return false
		}
	}
	return true
}

// Called with Host.mu held; workers never wait for a node callback under it.
func (h *Host) cleanupTerminalsLocked(generation uint64, col *collector) <-chan struct{} {
	var selected []*hostTerminal
	for _, t := range h.terminals {
		if (generation == 0 || t.intent.Generation == generation) && (col == nil || col == t.col) {
			t.cancel()
			selected = append(selected, t)
		}
	}
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), terminalCleanupWait)
		defer cancel()
		var workers sync.WaitGroup
		for _, t := range selected {
			workers.Add(1)
			go func(t *hostTerminal) {
				defer workers.Done()
				if t.stop(ctx, h) == nil {
					h.releaseTerminal(t)
				}
			}(t)
		}
		workers.Wait()
		close(done)
	}()
	return done
}

// Terminal references and freeze linearize under collector.mu. This method
// runs after upsertTool and before publishing the same notification. Host.mu
// is acquired only in collector -> Host order, never across a progress or
// Node owner callback.
func (h *Host) terminalContentLocked(c *collector, u acp.SessionUpdate) {
	if h.cfg.TerminalOwner == nil {
		return
	}
	items, supplied := u.Content.([]acp.ToolCallContent)
	if !supplied {
		if ptr, ok := u.Content.(*[]acp.ToolCallContent); ok && ptr != nil {
			items, supplied = *ptr, true
		}
	}
	id := string(u.ToolCallID)
	if id == "" && u.Title != nil {
		id = *u.Title
	}
	if id == "" {
		return
	}
	hasTerminal := false
	for _, item := range items {
		hasTerminal = hasTerminal || item.Type == acp.ToolCallContentTypeTerminal
	}
	if !hasTerminal && len(c.terminalTools[id]) == 0 {
		return
	}
	index, ok := c.toolIndex[id]
	if !ok {
		return
	}
	if c.terminalTools == nil {
		c.terminalTools = map[string][]string{}
		c.terminalToolBase = map[string]string{}
		c.terminalTexts = map[string]string{}
		c.terminalReleased = map[string]bool{}
	}
	old := c.terminalTools[id]
	if len(old) == 0 {
		c.terminalToolBase[id] = c.tools[index].Output
	}
	h.mu.Lock()
	if supplied {
		next := make([]string, 0, min(len(items), maxTerminals))
		seen := map[string]bool{}
		for _, item := range items {
			if item.Type != acp.ToolCallContentTypeTerminal || item.TerminalID == "" {
				continue
			}
			terminal := string(item.TerminalID)
			if len(terminal) > 128 || seen[terminal] {
				continue
			}
			seen[terminal] = true
			permitted := false
			if c.terminalReleased[terminal] {
				// A frozen snapshot belongs to its existing attachments, not a new
				// use of an invalidated handle or resurrection of a replaced ID.
				for _, previous := range old {
					permitted = permitted || previous == terminal
				}
			} else if live := h.terminals[terminal]; live != nil && live.col == c {
				permitted = true
			}
			if permitted {
				next = append(next, terminal)
			}
		}
		c.terminalTools[id] = next
		_, text := splitToolContent(items)
		c.terminalToolBase[id] = text
		if len(next) == 0 {
			c.tools[index].Output = text
			delete(c.terminalTools, id)
			delete(c.terminalToolBase, id)
		}
	}
	if raw := formatAny(u.RawOutput); raw != "" {
		if len(c.terminalTools[id]) == 0 {
			c.tools[index].Output = raw
		} else {
			c.terminalToolBase[id] = raw
		}
	}
	referenced := map[string]bool{}
	for _, ids := range c.terminalTools {
		for _, terminal := range ids {
			referenced[terminal] = true
		}
	}
	for terminal, text := range c.terminalTexts {
		if referenced[terminal] {
			continue
		}
		if c.terminalReleased[terminal] {
			if _, retained := h.terminalOutputBytes[c]; retained {
				h.terminalOutputBytes[c] -= len(text)
			}
		}
		delete(c.terminalTexts, terminal)
		delete(c.terminalReleased, terminal)
	}
	if h.terminalOutputBytes[c] == 0 {
		delete(h.terminalOutputBytes, c)
	}
	// A handle that has been marked released but not yet frozen still belongs
	// to this c.mu linearization point. Its original final output is immutable.
	live := make([]*hostTerminal, 0, maxTerminals)
	for terminal := range referenced {
		if t := h.terminals[terminal]; t != nil && t.col == c && !c.terminalReleased[terminal] {
			live = append(live, t)
		}
	}
	h.mu.Unlock()
	for _, t := range live {
		text, _, _ := t.snapshot()
		c.terminalTexts[t.intent.ID] = text
	}
	c.renderTerminalToolsLocked()
}

func (c *collector) terminalAttachedLocked(id string) bool {
	for _, ids := range c.terminalTools {
		for _, terminal := range ids {
			if terminal == id {
				return true
			}
		}
	}
	return false
}

func (c *collector) freezeTerminalLocked(t *hostTerminal, text string) int {
	if !c.terminalAttachedLocked(t.intent.ID) {
		delete(c.terminalTexts, t.intent.ID)
		return 0
	}
	c.terminalTexts[t.intent.ID] = text
	c.terminalReleased[t.intent.ID] = true
	c.renderTerminalToolsLocked()
	return len(text) // One immutable string is shared by its attachments.
}

func utf8Tail(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && text[start]&0xc0 == 0x80 {
		start++
	}
	return strings.Clone(text[start:])
}

func (c *collector) renderTerminalToolsLocked() {
	// Duplicate references do not multiply the transcript's terminal budget.
	remaining := maxTerminalOutputBytes
	for _, tool := range c.tools {
		ids, ok := c.terminalTools[tool.ID]
		if !ok {
			continue
		}
		index := c.toolIndex[tool.ID]
		base := c.terminalToolBase[tool.ID]
		var parts []string
		if base != "" {
			parts = append(parts, base)
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			separator := 0
			if len(parts) != 0 {
				separator = 1
			}
			text := utf8Tail(c.terminalTexts[id], remaining-separator)
			if text != "" {
				parts = append(parts, text)
				remaining -= len(text) + separator
			}
		}
		c.tools[index].Output = strings.Join(parts, "\n")
	}
}

func (h *Host) handleUpdate(col *collector, u acp.SessionUpdate) {
	col.handleWithTerminals(u, func() { h.terminalContentLocked(col, u) })
}

// Component callers may already have upserted their notification. The real
// ACP Update path uses handleUpdate to register before progress instead.
func (h *Host) terminalUpdate(col *collector, u acp.SessionUpdate) {
	if h.cfg.TerminalOwner == nil || (u.SessionUpdate != acp.SessionUpdateTypeToolCall && u.SessionUpdate != acp.SessionUpdateTypeToolCallUpdate) {
		return
	}
	col.mu.Lock()
	h.terminalContentLocked(col, u)
	col.mu.Unlock()
	col.publishTerminalProgress()
}
func (c *collector) refreshTerminal(t *hostTerminal) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalReleased[t.intent.ID] || !c.terminalAttachedLocked(t.intent.ID) {
		return false
	}
	text, _, _ := t.snapshot()
	c.terminalTexts[t.intent.ID] = text
	c.renderTerminalToolsLocked()
	return true
}

func (c *collector) publishTerminalProgress() {
	c.mu.Lock()
	p, fn := c.snapshot()
	c.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}
func (h *Host) watchTerminalOutput(t *hostTerminal, runtime terminalRuntime) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var revision uint64
	for {
		select {
		case <-runtime.Done():
			if t.col.refreshTerminal(t) {
				t.col.publishTerminalProgress()
			}
			return
		case <-ticker.C:
			current := t.output.Version()
			if current == revision {
				continue
			}
			revision = current
			if t.col.refreshTerminal(t) {
				t.col.publishTerminalProgress()
			}
		}
	}
}

// stopTerminalChildren is native-only. It cannot call a Node owner and is
// safe in transport observation, including after the ACP stream was lost.
func (p *localProcess) stopTerminalChildren() {
	p.mu.Lock()
	children := make([]terminalRuntime, 0, len(p.terminalProcesses))
	for child := range p.terminalProcesses {
		children = append(children, child)
	}
	p.mu.Unlock()
	for _, child := range children {
		go func(child terminalRuntime) {
			ctx, cancel := context.WithTimeout(context.Background(), terminalCleanupWait)
			defer cancel()
			_ = child.Stop(ctx)
		}(child)
	}
}
