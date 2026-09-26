package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

func (p *Peer) DesktopDeclaration() (platformconfig.Declaration, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return platformconfig.Declaration{}, nil
	}
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return platformconfig.Declaration{}, err
	}
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return platformconfig.Declaration{}, err
		}
		if version >= state.AppVersion {
			break
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return platformconfig.Declaration{}, ctx.Err()
		case <-timer.C:
		}
	}
	d, _, err := platformconfig.New(runtime.Ledger()).Load()
	return d, err
}

func (p *Peer) localDesktopStatus() (consoleapi.DesktopStatus, error) {
	if !desktop.IsManagedConfig(p.Options.ConfigPath) {
		return consoleapi.DesktopStatus{}, nil
	}
	d, err := p.DesktopDeclaration()
	if err != nil {
		return consoleapi.DesktopStatus{}, err
	}
	stateDir := filepath.Dir(p.Options.ConfigPath)
	// The workspace the guide asks about is this machine's own directory,
	// which is where every project it holds is made. Until the execution
	// service is up to say, the default project's home is the best answer
	// there is.
	workspace := p.WorkerWorkspaceRoot()
	if workspace == "" {
		_, home := p.desktopProject(d)
		workspace = home.Path
	}
	result := consoleapi.DesktopStatus{Enabled: true, NodeID: p.Config.NodeID, AgentCount: len(d.Agents), WorkspacePath: workspace,
		WorkspaceManaged: desktop.ManagedWorkspace(stateDir, workspace)}
	for id, item := range d.Agents {
		if item.Default {
			result.DefaultAgent = id
		}
		if item.Node == p.Config.NodeID {
			result.LocalAgentCount++
		}
	}
	progress, err := desktop.ReadSetup(stateDir)
	if err != nil {
		slog.Warn("desktop: setup progress unreadable, reopening the guide", "error", err)
		progress = desktop.SetupProgress{Step: desktop.SetupSteps[0]}
	}
	result.Setup = &consoleapi.DesktopSetup{Step: progress.Step, Done: progress.Done}
	result.SetupRequired = !progress.Done
	return result, nil
}

// desktopProject is the project a conversation starts in and its directory
// on this computer: the shared declaration's default, or — until the
// application has published one — the bootstrap configuration's.
func (p *Peer) desktopProject(d platformconfig.Declaration) (string, config.ProjectHome) {
	if id := config.DefaultProjectID(d.DefaultProject, d.Projects); id != "" {
		return id, d.Projects[id].Home
	}
	cfg, err := config.Load(p.Options.ConfigPath)
	if err != nil {
		return "", config.ProjectHome{}
	}
	id := config.DefaultProjectID(cfg.Gateway.DefaultProject, cfg.Projects)
	return id, cfg.Projects[id].Home
}

// desktopError answers a refusal of the owner's input as a bad request and
// anything else as this machine's failure.
func desktopError(w http.ResponseWriter, err error) {
	if desktop.IsInputError(err) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// serveDesktopSetup records the guide page to open next, and
// serveDesktopWorkspace moves the default project to a directory the owner
// chose on this computer. Both answer with the desktop status.
func (p *Peer) serveDesktopSetup(w http.ResponseWriter, r *http.Request) {
	var request consoleapi.DesktopSetupRequest
	if !decodeDesktopRequest(w, r, &request) {
		return
	}
	if err := desktop.SaveSetup(filepath.Dir(p.Options.ConfigPath), desktop.SetupProgress{Step: request.Step, Done: request.Done}); err != nil {
		desktopError(w, err)
		return
	}
	p.writeDesktopStatus(w)
}

func (p *Peer) serveDesktopWorkspace(w http.ResponseWriter, r *http.Request) {
	var request consoleapi.DesktopWorkspaceRequest
	if !decodeDesktopRequest(w, r, &request) {
		return
	}
	d, err := p.DesktopDeclaration()
	if err != nil {
		HTTPError(w, err)
		return
	}
	path, err := desktop.PrepareWorkspace(p.text.For(r.Context()), request.Path, filepath.Dir(p.Options.ConfigPath))
	if err != nil {
		desktopError(w, err)
		return
	}
	// The directory the owner chose is this machine's workspace: where it
	// keeps everything it works on, with each project a directory under
	// it. It is a fact about this computer, so it is recorded whatever the
	// default project happens to be.
	if err := p.setLocalWorkspaceRoot(r.Context(), path); err != nil {
		HTTPError(w, err)
		return
	}
	// The default project moves in with it, as any project on this machine
	// would: named under the workspace rather than being the workspace, so
	// the next project to arrive has somewhere to go that is not inside it.
	// A default project that lives on another machine is left where it is.
	id, current := p.desktopProject(d)
	if id != "" && (current.Node == "" || current.Node == p.Config.NodeID) && !strings.HasPrefix(current.Path, path+string(filepath.Separator)) {
		var result struct {
			OK bool `json:"ok"`
		}
		if err := p.applicationJSON(r.Context(), http.MethodPut, "/console/projects/"+url.PathEscape(id)+"/home", consoleapi.ProjectHomeRequest{Path: id}, &result); err != nil {
			HTTPError(w, err)
			return
		}
		if !result.OK {
			HTTPError(w, errors.New("the application did not accept the new directory"))
			return
		}
	}
	p.writeDesktopStatus(w)
}

// setLocalWorkspaceRoot records where this machine keeps its work. The
// execution service owns the choice — its own file carries it across a
// restart, and every directory question is answered from what it reports
// — so it is told first and the running application is told after.
func (p *Peer) setLocalWorkspaceRoot(ctx context.Context, root string) error {
	if p.worker != nil {
		if err := p.worker.SetWorkspaceRoot(root); err != nil {
			return err
		}
		// The hub answers directory questions from the advert it holds,
		// which was taken before the move; ask for a fresh one so the
		// next project lands in the new place.
		p.Mu.RLock()
		application := p.Application
		p.Mu.RUnlock()
		if application != nil && application.Admin != nil {
			application.Admin.SetLocalWorkspaceRoot(root)
			if application.Admin.Nodes != nil {
				if _, err := application.Admin.Nodes.Refresh(ctx, p.Config.NodeID); err != nil {
					slog.Warn("desktop: the execution service did not report its new workspace directory", "error", err)
				}
			}
		}
	}
	return nil
}

func (p *Peer) writeDesktopStatus(w http.ResponseWriter) {
	status, err := p.localDesktopStatus()
	if err != nil {
		HTTPError(w, err)
		return
	}
	WriteJSON(w, status)
}

func decodeDesktopRequest(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "one request object is required", http.StatusBadRequest)
		return false
	}
	return true
}

// serveDesktopLocal is reached only after the stable local gateway has checked
// its UI credential. Tool discovery always concerns this computer.
func (p *Peer) serveDesktopLocal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/console/desktop" && r.Method == http.MethodGet {
		status, err := p.localDesktopStatus()
		if err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, status)
		return
	}
	if !desktop.IsManagedConfig(p.Options.ConfigPath) {
		http.Error(w, "desktop setup is not available", http.StatusNotFound)
		return
	}
	switch {
	case r.URL.Path == "/console/desktop/setup" && r.Method == http.MethodPut:
		p.serveDesktopSetup(w, r)
		return
	case r.URL.Path == "/console/desktop/workspace" && r.Method == http.MethodPut:
		p.serveDesktopWorkspace(w, r)
		return
	case r.URL.Path != "/console/desktop/agents":
		http.Error(w, "desktop setup is not available", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		d, err := p.DesktopDeclaration()
		if err != nil {
			HTTPError(w, err)
			return
		}
		result := consoleapi.DesktopDiscovery{Agents: []consoleapi.DesktopAgentCandidate{}}
		for _, candidate := range p.worker.LocalAgentDiscovery().Agents {
			registered := false
			for _, item := range d.Agents {
				if item.Node == p.Config.NodeID && item.Harness == candidate.Harness {
					registered = true
				}
			}
			result.Agents = append(result.Agents, consoleapi.DesktopAgentCandidate{ID: candidate.ID, Name: candidate.Name, Harness: candidate.Harness, Executable: candidate.Executable, Installed: candidate.Installed, Requires: candidate.Requires, Registered: registered})
		}
		WriteJSON(w, result)
	case http.MethodPost:
		var request consoleapi.DesktopEnrollRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "one enrollment is required", http.StatusBadRequest)
			return
		}
		// The guide sends names, uses and which one is the default;
		// AgentIDs is the same choice from an older client, under the
		// tools' own IDs.
		requested := request.Agents
		if len(requested) == 0 {
			for _, id := range request.AgentIDs {
				requested = append(requested, consoleapi.DesktopEnrollAgent{CandidateID: id})
			}
		}
		if len(requested) == 0 {
			http.Error(w, p.text.For(r.Context()).T(i18n.AdminDesktopChooseAgent), http.StatusBadRequest)
			return
		}
		seen := make(map[string]bool, len(requested))
		for _, want := range requested {
			want.CandidateID = strings.TrimSpace(want.CandidateID)
			if want.CandidateID == "" || seen[want.CandidateID] {
				continue
			}
			seen[want.CandidateID] = true
			if err := p.enrollDesktopAgent(r.Context(), want); err != nil {
				HTTPError(w, err)
				return
			}
		}
		status, err := p.localDesktopStatus()
		if err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, status)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (p *Peer) enrollDesktopAgent(ctx context.Context, want consoleapi.DesktopEnrollAgent) error {
	candidateID := want.CandidateID
	d, err := p.DesktopDeclaration()
	if err != nil {
		return err
	}
	canonical, ok := agenttools.Lookup(candidateID)
	if !ok {
		return agenttools.ErrUnsupported
	}
	for _, item := range d.Agents {
		if item.Node == p.Config.NodeID && item.Harness == canonical.Harness {
			return nil
		}
	}
	state := p.worker.LocalAgentDiscovery()
	installed, err := p.worker.LocalEnrollAgent(ctx, agenttools.InstallRequest{CandidateID: candidateID, ExpectedRevision: state.Revision})
	if err != nil {
		return err
	}
	// A name the owner typed is the name they will call it by, so a
	// collision is reported rather than worked around. A tool enrolled
	// under its own ID may still be placed beside a namesake.
	agentID := strings.ToLower(strings.TrimSpace(want.AgentID))
	named := agentID != ""
	if !named {
		agentID = candidateID
	}
	if _, exists := d.Agents[agentID]; exists {
		if named {
			return errors.New(p.text.For(ctx).T(i18n.AdminAgentNameTakenRename, agentID))
		}
		suffix := p.Config.NodeID
		if len(suffix) > 10 {
			suffix = suffix[len(suffix)-10:]
		}
		agentID += "-" + suffix
	}
	var result struct {
		OK bool `json:"ok"`
	}
	err = p.applicationJSON(ctx, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: agentID, Harness: installed.Harness, Node: p.Config.NodeID, About: strings.TrimSpace(want.About), Default: want.Default}, &result)
	if err != nil {
		// The tool can already be installed even when declaration delivery was
		// interrupted. Only a matching shared Agent declaration confirms it.
		if current, readErr := p.DesktopDeclaration(); readErr == nil {
			if item, exists := current.Agents[agentID]; exists && item.Node == p.Config.NodeID && item.Harness == installed.Harness {
				return nil
			}
		}
		return err
	}
	if !result.OK {
		return errors.New("agent registration was not confirmed")
	}
	return nil
}

func (p *Peer) applicationJSON(ctx context.Context, method, endpoint string, input, output any) error {
	if !strings.HasPrefix(endpoint, "/console/") && endpoint != "/state" {
		return errors.New("invalid application request")
	}
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.UiURL+endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.UIToken)
	askIn(ctx, request.Header)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: p.localTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("application HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output)
}
