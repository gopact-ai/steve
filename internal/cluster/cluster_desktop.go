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
	_, home := p.desktopProject(d)
	result := consoleapi.DesktopStatus{Enabled: true, NodeID: p.Config.NodeID, AgentCount: len(d.Agents), WorkspacePath: home.Path,
		WorkspaceManaged: desktop.ManagedWorkspace(stateDir, home.Path)}
	for id, item := range d.Agents {
		if item.Default {
			result.DefaultAgent = id
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
	id, current := p.desktopProject(d)
	if err := desktop.CheckWorkspaceProject(id, current.Node); err != nil {
		desktopError(w, err)
		return
	}
	path, err := desktop.PrepareWorkspace(request.Path, filepath.Dir(p.Options.ConfigPath))
	if err != nil {
		desktopError(w, err)
		return
	}
	if current.Path != path {
		var result struct {
			OK bool `json:"ok"`
		}
		if err := p.applicationJSON(r.Context(), http.MethodPut, "/console/projects/"+url.PathEscape(id)+"/home", consoleapi.ProjectHomeRequest{Path: path}, &result); err != nil {
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
		for _, id := range request.AgentIDs {
			if err := p.enrollDesktopAgent(r.Context(), id); err != nil {
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

func (p *Peer) enrollDesktopAgent(ctx context.Context, candidateID string) error {
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
	agentID := candidateID
	if _, exists := d.Agents[agentID]; exists {
		suffix := p.Config.NodeID
		if len(suffix) > 10 {
			suffix = suffix[len(suffix)-10:]
		}
		agentID += "-" + suffix
	}
	var result struct {
		OK bool `json:"ok"`
	}
	err = p.applicationJSON(ctx, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: agentID, Harness: installed.Harness, Node: p.Config.NodeID}, &result)
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
