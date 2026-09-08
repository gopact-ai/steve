package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

// Every field in this first settings service is restart-applied. The boot
// snapshot stays separate from the desired file and is never rewritten here.
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
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
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
	return consoleapi.SettingsView{Revision: s.admin.settingsRevision(), Desired: d, Effective: e, PendingRestart: !reflect.DeepEqual(desired, s.applied), ApplyMode: "restart", Fields: fields}, nil
}

func (s *hubSettingsService) UpdateSettings(ctx context.Context, req consoleapi.SettingsUpdate) (consoleapi.SettingsView, error) {
	s.admin.Mu.Lock()
	defer s.admin.Mu.Unlock()
	ConfigMu.Lock()
	defer ConfigMu.Unlock()
	if req.BaseRevision == "" || req.BaseRevision != s.admin.settingsRevision() {
		return consoleapi.SettingsView{}, consoleapi.ErrSettingsConflict
	}
	if err := s.admin.Cfg.CheckFileRevision(s.admin.Path); err != nil {
		return consoleapi.SettingsView{}, errors.Join(consoleapi.ErrSettingsConflict, err)
	}
	candidate, err := s.admin.Cfg.PatchSettings(req.Settings)
	if err != nil {
		return consoleapi.SettingsView{}, err
	}
	if candidate.Gateway.OwnerID != s.admin.Cfg.Gateway.OwnerID {
		return consoleapi.SettingsView{}, errors.New("Owner identity must be changed through deployment configuration")
	}
	if err := candidate.ValidateChannels(); err != nil {
		return consoleapi.SettingsView{}, err
	}
	if reflect.DeepEqual(candidate.SettingsValues(), s.admin.Cfg.SettingsValues()) {
		return s.viewLocked()
	}
	saveErr := RedactChannelError(s.admin.persistConfigContext(ctx, candidate), s.admin.Cfg.Feishu.AppSecret, candidate.Feishu.AppSecret)
	if saveErr != nil && !config.Committed(saveErr) {
		if errors.Is(saveErr, config.ErrFileChanged) || errors.Is(saveErr, platformconfig.ErrConflict) {
			return consoleapi.SettingsView{}, errors.Join(consoleapi.ErrSettingsConflict, saveErr)
		}
		return consoleapi.SettingsView{}, saveErr
	}
	*s.admin.Cfg = *candidate
	view, err := s.viewLocked()
	if err != nil {
		return consoleapi.SettingsView{}, err
	}
	if saveErr != nil {
		view.Warning = saveErr.Error()
	}
	return view, nil
}

func (a *Service) settingsRevision() string {
	if a.ConfigRevision != nil {
		return a.ConfigRevision()
	}
	return a.Cfg.FileRevision()
}

func settingsFields() ([]consoleapi.SettingsField, error) {
	zero, one, maxSafe := int64(0), int64(1), int64(9_007_199_254_740_991)
	fields := []consoleapi.SettingsField{{Path: "gateway.locale", Type: "string", Enum: []string{"", "zh", "en"}, ApplyMode: "restart"}, {Path: "gateway.owner_id", Type: "string", ApplyMode: "restart"}, {Path: "gateway.task_max_turns", Type: "integer", Minimum: &zero, Maximum: &maxSafe, ApplyMode: "restart"}, {Path: "gateway.task_max_elapsed", Type: "duration", Unit: "duration", Minimum: &zero, ApplyMode: "restart"}}
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
