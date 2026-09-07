// Package platformconfig owns a cluster's shared work declarations and
// settings. Platform channel credentials stay in the private shared ledger;
// machine listeners, runtime commands and CLI credentials stay outside it.
package platformconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/ledger"
)

const document = "platform-configuration"

var (
	ErrConflict     = errors.New("shared configuration changed; reload before saving")
	ErrSealedShared = errors.New("sealed 项目仅支持独立单机实例；共享协调账本会复制项目正文")
)

type Declaration struct {
	Revision       uint64                    `json:"revision"`
	Settings       config.SettingsValues     `json:"settings"`
	Channels       config.ChannelSettings    `json:"channels"`
	Credentials    ChannelCredentials        `json:"channel_credentials"`
	Work           WorkPolicy                `json:"work"`
	DefaultProject string                    `json:"default_project,omitempty"`
	Home           config.ProjectHome        `json:"home"`
	Agents         map[string]config.Agent   `json:"agents"`
	Projects       map[string]config.Project `json:"projects"`
	Nodes          map[string]config.Node    `json:"nodes"`
}

// ChannelCredentials are private platform connection material. They are never
// part of SettingsValues or ChannelSettings returned to a user interface.
type ChannelCredentials struct {
	FeishuAppSecret string `json:"feishu_app_secret,omitempty"`
}

// WorkPolicy names behavior shared across coordinator changes. It contains
// no process address, credential, node capability or machine-local path.
type WorkPolicy struct {
	Planner              string          `json:"planner,omitempty"`
	OfflineReminderAfter config.Duration `json:"offline_reminder_after,omitempty"`
	DirectTransfer       bool            `json:"direct_transfer,omitempty"`
}

type LocalNode struct {
	ID     string
	Config config.Node
}

type Store struct{ book *ledger.Ledger }

func New(book *ledger.Ledger) *Store { return &Store{book: book} }

func (s *Store) Load() (Declaration, bool, error) {
	raw, ok, err := s.book.Document(document).Load()
	if err != nil || !ok {
		return Declaration{}, ok, err
	}
	var d Declaration
	if err := json.Unmarshal(raw, &d); err != nil {
		return Declaration{}, false, err
	}
	return d, true, validate(d)
}

func (s *Store) Bootstrap(ctx context.Context, cfg *config.Config, local LocalNode) (Declaration, error) {
	if cfg == nil {
		return Declaration{}, errors.New("application configuration is required")
	}
	if err := validateClassifications(cfg.Projects); err != nil {
		return Declaration{}, err
	}
	if current, ok, err := s.Load(); err != nil || ok {
		return current, err
	}
	d, err := FromLocal(cfg, local)
	if err != nil {
		return Declaration{}, err
	}
	return s.Save(ctx, 0, d)
}

func (s *Store) Save(ctx context.Context, expected uint64, next Declaration) (Declaration, error) {
	return s.SaveWithGuard(ctx, expected, next, nil)
}

// SaveWithGuard validates an originating tool grant in the same transaction
// as the declaration. A preceding authorization read cannot fence a late call.
func (s *Store) SaveWithGuard(ctx context.Context, expected uint64, next Declaration, guard func(*ledger.Tx) error) (Declaration, error) {
	next.Revision = expected + 1
	if next.Revision == 0 {
		return Declaration{}, errors.New("shared configuration revision exhausted")
	}
	if err := validate(next); err != nil {
		return Declaration{}, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return Declaration{}, err
	}
	err = s.book.Update(ctx, func(tx *ledger.Tx) error {
		if guard != nil {
			if err := guard(tx); err != nil {
				return err
			}
		}
		previous, ok, err := tx.LoadDocument(document)
		if err != nil {
			return err
		}
		var old Declaration
		if ok {
			if err := json.Unmarshal(previous, &old); err != nil {
				return err
			}
		}
		if old.Revision != expected {
			return ErrConflict
		}
		return tx.StoreDocument(document, raw)
	})
	if err != nil {
		return Declaration{}, err
	}
	return clone(next)
}

// FromLocal turns every location in an initial declaration into a physical
// node identity. It does not copy harness, external MCP or UI credentials.
func FromLocal(cfg *config.Config, local LocalNode) (Declaration, error) {
	if cfg == nil || local.ID == "" || local.Config.Addr == "" || local.Config.Token == "" {
		return Declaration{}, errors.New("shared configuration requires an identified local execution service")
	}
	d, err := declaration(cfg, config.ProjectHome{Node: local.ID, Path: cfg.Gateway.HomePath})
	if err != nil {
		return Declaration{}, err
	}
	if d.Nodes == nil {
		d.Nodes = map[string]config.Node{}
	}
	d.Nodes[local.ID] = local.Config
	normalize := func(node string) string {
		if node == "" {
			return local.ID
		}
		return node
	}
	for id, a := range d.Agents {
		a.Node = normalize(a.Node)
		d.Agents[id] = a
	}
	for id, p := range d.Projects {
		p.Home.Node = normalize(p.Home.Node)
		for i := range p.Workspaces {
			p.Workspaces[i].Node = normalize(p.Workspaces[i].Node)
			if p.Workspaces[i].Source != "" {
				p.Workspaces[i].Source = normalize(p.Workspaces[i].Source)
			}
		}
		for i := range p.DurablePlaces {
			p.DurablePlaces[i] = normalize(p.DurablePlaces[i])
		}
		if len(p.DurablePlaces) == 0 {
			p.DurablePlaces = []string{local.ID}
		}
		d.Projects[id] = p
	}
	return d, validate(d)
}

func (d Declaration) Apply(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("application configuration is required")
	}
	if err := validate(d); err != nil {
		return err
	}
	owned, err := clone(d)
	if err != nil {
		return err
	}
	cfg.Agents, cfg.Projects, cfg.Nodes = owned.Agents, owned.Projects, owned.Nodes
	owned.applySettings(cfg)
	cfg.RuntimeHome = &owned.Home
	return nil
}

func (d Declaration) WithCandidate(cfg *config.Config) (Declaration, error) {
	next, err := declaration(cfg, d.Home)
	if err != nil {
		return Declaration{}, err
	}
	next.Revision = d.Revision
	return next, validate(next)
}

func declaration(cfg *config.Config, home config.ProjectHome) (Declaration, error) {
	if cfg == nil {
		return Declaration{}, errors.New("application configuration is required")
	}
	settings := cfg.SettingsValues()
	settings.Gateway.OwnerID = cfg.EffectiveOwnerID()
	return clone(Declaration{Settings: settings, Channels: cfg.ChannelSettings(), Credentials: ChannelCredentials{FeishuAppSecret: cfg.Feishu.AppSecret},
		Work: WorkPolicy{Planner: cfg.Gateway.Planner, OfflineReminderAfter: cfg.Gateway.OfflineReminderAfter, DirectTransfer: cfg.Gateway.DirectTransfer}, DefaultProject: cfg.Gateway.DefaultProject,
		Home: home, Agents: cfg.Agents, Projects: cfg.Projects, Nodes: cfg.Nodes})
}

func (d Declaration) applySettings(cfg *config.Config) {
	policy := d.Settings.Gateway
	cfg.Gateway.OwnerID, cfg.Gateway.Locale = policy.OwnerID, policy.Locale
	cfg.Gateway.TaskMaxTurns, cfg.Gateway.TaskMaxElapsed, cfg.Gateway.PromptTimeout = policy.TaskMaxTurns, policy.TaskMaxElapsed, policy.PromptTimeout
	cfg.Policies = d.Settings.Policies
	cfg.Gateway.Planner, cfg.Gateway.OfflineReminderAfter, cfg.Gateway.DirectTransfer = d.Work.Planner, d.Work.OfflineReminderAfter, d.Work.DirectTransfer
	cfg.Gateway.DefaultProject, cfg.Gateway.DefaultChannel = d.DefaultProject, d.Channels.DefaultChannel
	f := d.Channels.Feishu
	enabled := f.Enabled
	cfg.Feishu = config.Feishu{Enabled: &enabled, AppID: f.AppID, AppSecret: d.Credentials.FeishuAppSecret, Domain: f.Domain, OwnerOpenID: f.OwnerOpenID, GroupPolicy: f.GroupPolicy, AllowUnmentioned: f.AllowUnmentioned, AllowedSenders: append([]string{}, f.AllowedSenders...), BlockedSenders: append([]string{}, f.BlockedSenders...)}
}

func validate(d Declaration) error {
	if err := validateClassifications(d.Projects); err != nil {
		return err
	}
	if err := d.Settings.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(d.Settings.Gateway.OwnerID) == "" {
		return errors.New("shared owner identity is required")
	}
	if !d.Channels.Console.Enabled || d.Channels.Console.OwnerID != d.Settings.Gateway.OwnerID || d.Channels.Feishu.AppSecretConfigured != (d.Credentials.FeishuAppSecret != "") {
		return errors.New("shared channel identity or credential status is inconsistent")
	}
	channelConfig := &config.Config{}
	d.applySettings(channelConfig)
	if err := channelConfig.ValidateChannels(); err != nil {
		return err
	}
	if d.Work.Planner != "" {
		if _, ok := d.Agents[d.Work.Planner]; !ok {
			return errors.New("shared planner references an unknown agent")
		}
	}
	known := func(id string) bool { _, ok := d.Nodes[id]; return id != "" && ok }
	if !known(d.Home.Node) || d.Home.Path == "" {
		return errors.New("home workspace requires a physical node and path")
	}
	for id, node := range d.Nodes {
		if id == "" || node.Addr == "" || node.Token == "" {
			return fmt.Errorf("node %q lacks a connection identity", id)
		}
	}
	for id, agent := range d.Agents {
		if !known(agent.Node) {
			return fmt.Errorf("agent %q must name a known physical node", id)
		}
	}
	for id, project := range d.Projects {
		if !known(project.Home.Node) {
			return fmt.Errorf("project %q must name a known physical home node", id)
		}
		for _, workspace := range project.Workspaces {
			if !known(workspace.Node) {
				return fmt.Errorf("project %q has an unknown workspace node", id)
			}
		}
		for _, place := range project.DurablePlaces {
			if !known(place) {
				return fmt.Errorf("project %q has an unknown durable node", id)
			}
		}
	}
	_, err := (&config.Config{Agents: d.Agents}).AgentCatalog()
	return err
}

// Shared application documents contain task, conversation and memory text.
// Sealed data cannot enter an all-member ledger, even in its first single-peer
// phase, because a later join replays its complete log and database history.
func validateClassifications(projects map[string]config.Project) error {
	for id, item := range projects {
		switch item.Level {
		case "", "public", "internal", "restricted":
		case "sealed":
			return fmt.Errorf("%w: %s", ErrSealedShared, id)
		default:
			return fmt.Errorf("project %q has an invalid shared data level", id)
		}
	}
	return nil
}

func clone(d Declaration) (Declaration, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return Declaration{}, err
	}
	var result Declaration
	err = json.Unmarshal(raw, &result)
	return result, err
}
