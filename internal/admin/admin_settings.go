package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/approval"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

// Desired values are published only after persistence commits. Deployments
// without runtime policy consumers retain an honest restart-applied snapshot.
type hubSettingsService struct {
	admin     *Service
	applied   config.SettingsValues
	effective config.SettingsValues
}

func NewSettings(admin *Service, startup *config.Config) *hubSettingsService {
	values := startup.SettingsValues()
	effective := values
	effective.Gateway.Locale = startup.EffectiveLocale()
	effective.Gateway.OwnerID = startup.EffectiveOwnerID()
	return &hubSettingsService{admin: admin, applied: values, effective: effective}
}

func (s *hubSettingsService) Settings(context.Context) (consoleapi.SettingsView, error) {
	s.admin.configStore().RLock()
	defer s.admin.configStore().RUnlock()
	return s.viewLocked()
}

func (s *hubSettingsService) viewLocked() (consoleapi.SettingsView, error) {
	desired := s.admin.Cfg.SettingsValues()
	d, err := json.Marshal(desired)
	if err != nil {
		return consoleapi.SettingsView{}, fmt.Errorf("encode desired settings: %w", err)
	}
	e, err := json.Marshal(s.effective)
	if err != nil {
		return consoleapi.SettingsView{}, fmt.Errorf("encode effective settings: %w", err)
	}
	fields, err := settingsFields()
	if err != nil {
		return consoleapi.SettingsView{}, err
	}
	mode := "restart"
	pending := !reflect.DeepEqual(desired, s.applied)
	if s.admin.RuntimeSettings != nil {
		mode = "live"
		// Channel edits pin implicit locale/owner defaults without changing
		// their meaning. Compare effective values, not their file spelling.
		comparable := desired
		comparable.Gateway.Locale = s.admin.Cfg.EffectiveLocale()
		comparable.Gateway.OwnerID = s.admin.Cfg.EffectiveOwnerID()
		pending = !reflect.DeepEqual(comparable, s.effective)
		for i := range fields {
			switch fields[i].Path {
			case "gateway.owner_id":
				fields[i].ApplyMode = "deployment"
			case "gateway.task_max_turns", "gateway.task_max_elapsed":
				fields[i].ApplyMode = "next_task"
			default:
				fields[i].ApplyMode = "next_operation"
			}
		}
	}
	return consoleapi.SettingsView{Revision: s.admin.settingsRevision(), Desired: d, Effective: e, PendingRestart: pending, ApplyMode: mode, Fields: fields}, nil
}

func (s *hubSettingsService) UpdateSettings(ctx context.Context, req consoleapi.SettingsUpdate) (consoleapi.SettingsView, error) {
	s.admin.Mu.Lock()
	defer s.admin.Mu.Unlock()
	var previous, oldSecret, newSecret string
	var view consoleapi.SettingsView
	var viewErr error
	saving := false
	saveErr := s.admin.updateConfigThen(ctx, func(c *config.Config) error {
		if req.BaseRevision == "" || req.BaseRevision != s.admin.settingsRevision() {
			return consoleapi.ErrSettingsConflict
		}
		if err := c.CheckFileRevision(s.admin.Path); err != nil {
			return errors.Join(consoleapi.ErrSettingsConflict, err)
		}
		candidate, err := c.PatchSettings(req.Settings)
		if err != nil {
			return err
		}
		if candidate.Gateway.OwnerID != c.Gateway.OwnerID {
			return errors.New("Owner identity must be changed through deployment configuration")
		}
		if err := candidate.ValidateChannels(); err != nil {
			return err
		}
		if reflect.DeepEqual(candidate.SettingsValues(), c.SettingsValues()) {
			return errUnchanged
		}
		previous, oldSecret, newSecret = c.Gateway.DefaultApproval, c.Feishu.AppSecret, candidate.Feishu.AppSecret
		*c = *candidate
		saving = true
		return nil
	}, func(saved *config.Config) {
		// Approval is read at session open through the agent catalog.
		// Publish it separately from policies sampled at operation
		// boundaries.
		s.applyApproval(previous)
		if s.admin.RuntimeSettings != nil {
			s.admin.RuntimeSettings.Publish(saved)
			// Approval has a separate catalog publication boundary. Keep a
			// failed publication pending rather than claiming the new
			// catalog is in use.
			approvalApplied := s.applied.Gateway.DefaultApproval
			approvalEffective := s.effective.Gateway.DefaultApproval
			s.applied = saved.SettingsValues()
			s.effective = s.admin.RuntimeSettings.Load()
			s.applied.Gateway.DefaultApproval = approvalApplied
			s.effective.Gateway.DefaultApproval = approvalEffective
		}
		view, viewErr = s.viewLocked()
	})
	if !saving {
		if saveErr != nil {
			return consoleapi.SettingsView{}, saveErr
		}
		return s.Settings(ctx)
	}
	saveErr = RedactChannelError(saveErr, oldSecret, newSecret)
	if saveErr != nil && !config.Committed(saveErr) {
		if errors.Is(saveErr, config.ErrFileChanged) || errors.Is(saveErr, platformconfig.ErrConflict) {
			return consoleapi.SettingsView{}, errors.Join(consoleapi.ErrSettingsConflict, saveErr)
		}
		return consoleapi.SettingsView{}, saveErr
	}
	if viewErr != nil {
		return consoleapi.SettingsView{}, viewErr
	}
	if saveErr != nil {
		view.Warning = saveErr.Error()
	}
	return view, nil
}

// applyApproval publishes a catalog carrying the new approval stance, then
// records it as both applied and effective so nothing asks for a restart it
// does not need and the console reads back the stance now in force. A
// catalog that will not build is left alone and remains visibly pending.
func (s *hubSettingsService) applyApproval(previous string) {
	if s.admin.Cfg.Gateway.DefaultApproval == previous || s.admin.Catalog == nil {
		return
	}
	prepared, err := s.admin.Cfg.AgentCatalog()
	if err != nil {
		slog.Error(fmt.Sprintf("steve: default approval waits for a restart: %v", err), "approval", s.admin.Cfg.Gateway.DefaultApproval)
		return
	}
	s.admin.Catalog.Publish(prepared)
	s.applied.Gateway.DefaultApproval = s.admin.Cfg.Gateway.DefaultApproval
	s.effective.Gateway.DefaultApproval = s.admin.Cfg.Gateway.DefaultApproval
	slog.Info(fmt.Sprintf("steve: default approval is now %q", s.admin.Cfg.Gateway.DefaultApproval), "approval", s.admin.Cfg.Gateway.DefaultApproval)
}

func (a *Service) settingsRevision() string {
	if a.ConfigRevision != nil {
		return a.ConfigRevision()
	}
	return a.Cfg.FileRevision()
}

func settingsFields() ([]consoleapi.SettingsField, error) {
	zero, one, maxSafe := int64(0), int64(1), int64(9_007_199_254_740_991)
	fields := []consoleapi.SettingsField{{Path: "gateway.locale", Type: "string", Enum: []string{"", "zh", "en"}, ApplyMode: "restart"}, {Path: "gateway.default_approval", Type: "string", Enum: approval.Intents(), ApplyMode: "live"}, {Path: "gateway.owner_id", Type: "string", ApplyMode: "restart"}, {Path: "gateway.task_max_turns", Type: "integer", Minimum: &zero, Maximum: &maxSafe, ApplyMode: "restart"}, {Path: "gateway.task_max_elapsed", Type: "duration", Unit: "duration", Minimum: &zero, ApplyMode: "restart"}}
	fields = append(fields, consoleapi.SettingsField{Path: "policies.landing.conflicts", Type: "string", Enum: []string{config.ConflictsByAgent, config.ConflictsManual}, ApplyMode: "restart"})
	for _, path := range []string{"gateway.prompt_timeout", "policies.execution.step_timeout", "policies.execution.verify_timeout", "policies.planning.timeout", "policies.review.timeout"} {
		fields = append(fields, consoleapi.SettingsField{Path: path, Type: "duration", Unit: "duration", Minimum: &one, ApplyMode: "restart"})
	}
	for _, row := range []struct{ path, unit string }{{"planning.attempts", "count"}, {"snapshot.max_files", "count"}, {"snapshot.max_bytes", "bytes"}, {"snapshot.max_file_bytes", "bytes"}, {"review.max_changes", "count"}, {"review.max_diff_bytes", "bytes"}, {"review.max_file_bytes", "bytes"}, {"review.max_entries", "count"}} {
		fields = append(fields, consoleapi.SettingsField{Path: "policies." + row.path, Type: "integer", Unit: row.unit, Minimum: &one, Maximum: &maxSafe, ApplyMode: "restart"})
	}
	encoded, err := json.Marshal((&config.Config{}).SettingsValues())
	if err != nil {
		return nil, fmt.Errorf("encode default settings: %w", err)
	}
	var defaults map[string]any
	if err := json.Unmarshal(encoded, &defaults); err != nil {
		return nil, fmt.Errorf("decode default settings: %w", err)
	}
	for i := range fields {
		var value any = defaults
		for _, part := range strings.Split(fields[i].Path, ".") {
			object, ok := value.(map[string]any)
			if !ok {
				value = nil
				break
			}
			value = object[part]
		}
		if fields[i].Default, err = json.Marshal(value); err != nil {
			return nil, fmt.Errorf("encode default of %s: %w", fields[i].Path, err)
		}
	}
	return fields, nil
}
