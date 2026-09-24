package admin

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/channelsettings"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

type hubChannelsService struct {
	admin         *Service
	applied       channelsettings.Settings
	appliedSecret string
	runtimeError  string
	accessUpdater func(config.Feishu)
}

// BindAccessUpdater binds a successfully initialized runtime channel and
// immediately publishes its applied startup policy, not pending declarations.
// The callback runs synchronously under the configuration lock to preserve
// publication order; it must be nonblocking and must not call back into
// administration services.
// Its payload contains only the four access fields, never channel credentials.
// Passing nil removes the consumer and restores restart-only semantics.
func (s *hubChannelsService) BindAccessUpdater(update func(config.Feishu)) {
	s.admin.configStore().Lock()
	defer s.admin.configStore().Unlock()
	s.accessUpdater = update
	if update != nil {
		update(channelAccess(s.applied.Feishu))
	}
}

func channelAccess(f channelsettings.FeishuSetting) config.Feishu {
	return config.Feishu{
		GroupPolicy:      f.GroupPolicy,
		AllowUnmentioned: f.AllowUnmentioned,
		AllowedSenders:   append([]string{}, f.AllowedSenders...),
		BlockedSenders:   append([]string{}, f.BlockedSenders...),
	}
}

func (s *hubChannelsService) SetRuntimeError(message string) {
	s.admin.configStore().Lock()
	defer s.admin.configStore().Unlock()
	s.runtimeError = message
}

func NewChannels(admin *Service, startup *config.Config) *hubChannelsService {
	return &hubChannelsService{admin: admin, applied: startup.ChannelSettings(), appliedSecret: startup.Feishu.AppSecret}
}

func (s *hubChannelsService) Channels(context.Context) (consoleapi.ChannelsView, error) {
	s.admin.configStore().RLock()
	defer s.admin.configStore().RUnlock()
	return s.viewLocked(), nil
}

func (s *hubChannelsService) viewLocked() consoleapi.ChannelsView {
	desired := s.admin.Cfg.ChannelSettings()
	// A secret rotation is pending even if configured stays true in both
	// public views. Compare privately without returning a secret digest.
	pending := !reflect.DeepEqual(desired, s.applied) || s.admin.Cfg.Feishu.AppSecret != s.appliedSecret
	mode := "restart"
	var liveFields []string
	if s.accessUpdater != nil {
		mode = "mixed"
		liveFields = []string{"feishu.group_policy", "feishu.allow_unmentioned", "feishu.allowed_senders", "feishu.blocked_senders"}
	}
	return consoleapi.ChannelsView{Revision: s.admin.settingsRevision(), Desired: desired, Effective: cloneChannelSettings(s.applied), PendingRestart: pending, ApplyMode: mode, LiveFields: liveFields, RuntimeError: s.runtimeError}
}

func cloneChannelSettings(in channelsettings.Settings) channelsettings.Settings {
	in.Feishu.AllowedSenders = append([]string{}, in.Feishu.AllowedSenders...)
	in.Feishu.BlockedSenders = append([]string{}, in.Feishu.BlockedSenders...)
	return in
}

func (s *hubChannelsService) UpdateChannels(ctx context.Context, req consoleapi.ChannelsUpdate) (consoleapi.ChannelsView, error) {
	s.admin.Mu.Lock()
	defer s.admin.Mu.Unlock()
	s.admin.configStore().Lock()
	defer s.admin.configStore().Unlock()
	if req.BaseRevision == "" || req.BaseRevision != s.admin.settingsRevision() {
		return consoleapi.ChannelsView{}, consoleapi.ErrSettingsConflict
	}
	if err := s.admin.Cfg.CheckFileRevision(s.admin.Path); err != nil {
		return consoleapi.ChannelsView{}, errors.Join(consoleapi.ErrSettingsConflict, err)
	}
	candidate, err := s.admin.Cfg.PatchChannels(req.Channels)
	if err != nil {
		return consoleapi.ChannelsView{}, err
	}
	if reflect.DeepEqual(candidate.Feishu, s.admin.Cfg.Feishu) && reflect.DeepEqual(candidate.Gateway, s.admin.Cfg.Gateway) {
		return s.viewLocked(), nil
	}
	saveErr := s.admin.persistConfigContext(ctx, candidate)
	saveErr = RedactChannelError(saveErr, s.admin.Cfg.Feishu.AppSecret, candidate.Feishu.AppSecret)
	if saveErr != nil && !config.Committed(saveErr) {
		if errors.Is(saveErr, config.ErrFileChanged) || errors.Is(saveErr, platformconfig.ErrConflict) {
			return consoleapi.ChannelsView{}, errors.Join(consoleapi.ErrSettingsConflict, saveErr)
		}
		return consoleapi.ChannelsView{}, saveErr
	}
	*s.admin.Cfg = *candidate
	if s.accessUpdater != nil {
		next := candidate.ChannelSettings().Feishu
		s.accessUpdater(channelAccess(next))
		// Connection, credentials and identity remain the startup snapshot.
		// Only the policy accepted by the runtime consumer becomes applied.
		s.applied.Feishu.GroupPolicy = next.GroupPolicy
		s.applied.Feishu.AllowUnmentioned = next.AllowUnmentioned
		s.applied.Feishu.AllowedSenders = next.AllowedSenders
		s.applied.Feishu.BlockedSenders = next.BlockedSenders
	}
	view := s.viewLocked()
	if saveErr != nil {
		view.Warning = saveErr.Error()
	}
	return view, nil
}

type channelWriteError struct {
	cause error
	text  string
}

func (e *channelWriteError) Error() string { return e.text }

func (e *channelWriteError) Unwrap() error { return e.cause }

func RedactChannelError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return &channelWriteError{cause: err, text: text}
}
