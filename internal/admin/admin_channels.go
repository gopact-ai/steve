package admin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/gopact-ai/steve/internal/channelsettings"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

type hubChannelsService struct {
	admin *Service
	// mu guards the fields below. When both mu and the configuration
	// store are held, the store is taken first.
	mu            sync.Mutex
	applied       channelsettings.Settings
	appliedSecret string
	runtimeError  string
	startupRetry  *consoleapi.ChannelStartupRetry
	accessUpdater func(config.Feishu)
}

// BindAccessUpdater binds a successfully initialized runtime channel and
// immediately publishes its applied startup policy, not pending declarations.
// The callback runs synchronously under the service's lock, which also
// orders it with the publication of saved channels; it must be nonblocking
// and must not call back into administration services.
// Its payload contains only the four access fields, never channel credentials.
// Passing nil removes the consumer and restores restart-only semantics.
func (s *hubChannelsService) BindAccessUpdater(update func(config.Feishu)) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// SetRuntimeError reports a channel that stopped trying; it replaces a
// startup retry.
func (s *hubChannelsService) SetRuntimeError(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeError = message
	s.startupRetry = nil
}

// SetStartupRetry reports a channel startup that will be retried; nil
// reports none.
func (s *hubChannelsService) SetStartupRetry(retry *consoleapi.ChannelStartupRetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startupRetry = nil
	if retry != nil {
		copied := *retry
		s.startupRetry = &copied
	}
}

func NewChannels(admin *Service, startup *config.Config) *hubChannelsService {
	return &hubChannelsService{admin: admin, applied: startup.ChannelSettings(), appliedSecret: startup.Feishu.AppSecret}
}

func (s *hubChannelsService) Channels(context.Context) (consoleapi.ChannelsView, error) {
	s.admin.ConfigStore.rlock()
	defer s.admin.ConfigStore.runlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked(), nil
}

// viewLocked runs with the configuration held still and s.mu held.
func (s *hubChannelsService) viewLocked() consoleapi.ChannelsView {
	desired := s.admin.cfg().ChannelSettings()
	// A secret rotation is pending even if configured stays true in both
	// public views. Compare privately without returning a secret digest.
	pending := !reflect.DeepEqual(desired, s.applied) || s.admin.cfg().Feishu.AppSecret != s.appliedSecret
	mode := "restart"
	var liveFields []string
	if s.accessUpdater != nil {
		mode = "mixed"
		liveFields = []string{"feishu.group_policy", "feishu.allow_unmentioned", "feishu.allowed_senders", "feishu.blocked_senders"}
	}
	return consoleapi.ChannelsView{Revision: s.admin.settingsRevision(s.admin.cfg()), Desired: desired, Effective: cloneChannelSettings(s.applied), PendingRestart: pending, ApplyMode: mode, LiveFields: liveFields, RuntimeError: s.runtimeError, StartupRetry: s.startupRetryLocked()}
}

func (s *hubChannelsService) startupRetryLocked() *consoleapi.ChannelStartupRetry {
	if s.startupRetry == nil {
		return nil
	}
	copied := *s.startupRetry
	return &copied
}

func cloneChannelSettings(in channelsettings.Settings) channelsettings.Settings {
	in.Feishu.AllowedSenders = append([]string{}, in.Feishu.AllowedSenders...)
	in.Feishu.BlockedSenders = append([]string{}, in.Feishu.BlockedSenders...)
	return in
}

func (s *hubChannelsService) UpdateChannels(ctx context.Context, req consoleapi.ChannelsUpdate) (consoleapi.ChannelsView, error) {
	s.admin.Mu.Lock()
	defer s.admin.Mu.Unlock()
	var oldSecret, newSecret string
	var view consoleapi.ChannelsView
	saving := false
	saveErr := s.admin.updateConfigThen(ctx, func(c *config.Config) error {
		if req.BaseRevision == "" || req.BaseRevision != s.admin.settingsRevision(c) {
			return consoleapi.ErrSettingsConflict
		}
		if err := c.CheckFileRevision(s.admin.Path); err != nil {
			return errors.Join(consoleapi.ErrSettingsConflict, err)
		}
		candidate, err := c.PatchChannels(req.Channels)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(candidate.Feishu, c.Feishu) && reflect.DeepEqual(candidate.Gateway, c.Gateway) {
			return errUnchanged
		}
		oldSecret, newSecret = c.Feishu.AppSecret, candidate.Feishu.AppSecret
		*c = *candidate
		saving = true
		return nil
	}, func(saved *config.Config) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.accessUpdater != nil {
			next := saved.ChannelSettings().Feishu
			s.accessUpdater(channelAccess(next))
			// Connection, credentials and identity remain the startup
			// snapshot. Only the policy accepted by the runtime consumer
			// becomes applied.
			s.applied.Feishu.GroupPolicy = next.GroupPolicy
			s.applied.Feishu.AllowUnmentioned = next.AllowUnmentioned
			s.applied.Feishu.AllowedSenders = next.AllowedSenders
			s.applied.Feishu.BlockedSenders = next.BlockedSenders
		}
		view = s.viewLocked()
	})
	if !saving {
		if saveErr != nil {
			return consoleapi.ChannelsView{}, saveErr
		}
		return s.Channels(ctx)
	}
	saveErr = RedactChannelError(saveErr, oldSecret, newSecret)
	if saveErr != nil && !config.Committed(saveErr) {
		if errors.Is(saveErr, config.ErrFileChanged) || errors.Is(saveErr, platformconfig.ErrConflict) {
			return consoleapi.ChannelsView{}, errors.Join(consoleapi.ErrSettingsConflict, saveErr)
		}
		return consoleapi.ChannelsView{}, saveErr
	}
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
