package admin

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

type hubChannelsService struct {
	admin         *Service
	applied       config.ChannelSettings
	appliedSecret string
	runtimeError  string
}

func (s *hubChannelsService) SetRuntimeError(message string) {
	ConfigMu.Lock()
	defer ConfigMu.Unlock()
	s.runtimeError = message
}

func NewChannels(admin *Service, startup *config.Config) *hubChannelsService {
	return &hubChannelsService{admin: admin, applied: startup.ChannelSettings(), appliedSecret: startup.Feishu.AppSecret}
}

func (s *hubChannelsService) Channels(context.Context) (consoleapi.ChannelsView, error) {
	ConfigMu.RLock()
	defer ConfigMu.RUnlock()
	return s.viewLocked(), nil
}

func (s *hubChannelsService) viewLocked() consoleapi.ChannelsView {
	desired := s.admin.Cfg.ChannelSettings()
	// A secret rotation is pending even if configured stays true in both
	// public views. Compare privately without returning a secret digest.
	pending := !reflect.DeepEqual(desired, s.applied) || s.admin.Cfg.Feishu.AppSecret != s.appliedSecret
	return consoleapi.ChannelsView{Revision: s.admin.settingsRevision(), Desired: desired, Effective: cloneChannelSettings(s.applied), PendingRestart: pending, ApplyMode: "restart", RuntimeError: s.runtimeError}
}

func cloneChannelSettings(in config.ChannelSettings) config.ChannelSettings {
	in.Feishu.AllowedSenders = append([]string{}, in.Feishu.AllowedSenders...)
	in.Feishu.BlockedSenders = append([]string{}, in.Feishu.BlockedSenders...)
	return in
}

func (s *hubChannelsService) UpdateChannels(ctx context.Context, req consoleapi.ChannelsUpdate) (consoleapi.ChannelsView, error) {
	s.admin.Mu.Lock()
	defer s.admin.Mu.Unlock()
	ConfigMu.Lock()
	defer ConfigMu.Unlock()
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
