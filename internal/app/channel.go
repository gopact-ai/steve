package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

func runChannel(boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, work executionAssembly, projection readModelAssembly, management administrationAssembly, channels channelsAssembly) error {
	background := boot.Background()
	// The console is already accepting settings writes. Onboarding belongs
	// to this channel activation, not a later pending connection identity.
	startup := channels.Startup()
	ctx := boot.Context()
	store := storage.Store()
	profile := identity.Profile()
	catalogText := work.CatalogText()
	coordinator := work.Coordinator()
	view := projection.View()
	channelSettings := management.ChannelSettings()
	channel := channels.Channel()

	if channel == nil {
		slog.Info("steve: console-only hub ready")
		<-ctx.Done()
		return nil
	}
	background.Go(func(ctx context.Context) {
		// Onboarding writes to the owner, so it waits for a verified channel.
		select {
		case <-channel.Ready():
		case <-ctx.Done():
			return
		}
		timeout := startup.timeout
		if settings := boot.Settings(); settings != nil {
			timeout = time.Duration(settings.Load().Gateway.PromptTimeout)
		}
		onboardCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := onboard.Start(onboardCtx, onboard.Request{
			Owner:   startup.owner,
			Home:    startup.home,
			Reader:  profile.Home,
			Store:   store,
			Catalog: catalogText,
			Handle: func(ctx context.Context, req onboard.TurnRequest) (onboard.TurnResult, error) {
				result, err := coordinator.Handle(ctx, turn.Request{
					Channel:        "feishu",
					ConversationID: req.ConversationID,
					Input:          req.Input,
					SenderOpenID:   req.SenderOpenID,
					ChatType:       protocol.ParseChatType(req.ChatType),
				})
				if err != nil {
					return onboard.TurnResult{}, err
				}
				return onboard.TurnResult{Text: result.Text}, nil
			},
			Send: func(ctx context.Context, receiveID, text string) (string, error) {
				sent, err := channel.Send(ctx, receiveID, text)
				if err != nil {
					return "", err
				}
				return sent.ChatID, nil
			},
		}); err != nil {
			slog.Error(fmt.Sprintf("steve: onboard: %v", err))
		}
	})

	slog.Info("steve: starting Feishu long connection")
	err := channel.Start(ctx)
	channelSettings.BindAccessUpdater(nil)
	if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		// Only a failure that waiting will not fix reaches here; one that
		// may pass is retried inside Start.
		cause := adminsvc.RedactChannelError(err, boot.Config().Feishu.AppSecret)
		slog.Error(fmt.Sprintf("steve: Feishu connection failed; Console remains available: %v", cause))
		// Keys: channel.
		view.Observe("channel.error", "feishu", "Feishu connection failed; check channel credentials and restart the Hub", map[string]string{"channel": "feishu"})
		channelSettings.SetRuntimeError(fmt.Sprintf("Feishu connection failed: %v; check the channel configuration and restart the Hub", cause))
		<-ctx.Done()
	}
	return nil
}
