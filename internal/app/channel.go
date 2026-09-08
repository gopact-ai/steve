package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/turn"
)

func runChannel(boot runtimeAssembly, storage ledgerAssembly, identity homeAssembly, work executionAssembly, projection readModelAssembly, management administrationAssembly, channels channelsAssembly) error {
	background := boot.Background()
	cfg := boot.Config()
	ctx := boot.Context()
	store := storage.Store()
	profile := identity.Profile()
	catalogText := work.CatalogText()
	coordinator := work.Coordinator()
	view := projection.View()
	channelSettings := management.ChannelSettings()
	channel := channels.Channel()

	if channel == nil {
		log.Printf("steve: console-only hub ready")
		<-ctx.Done()
		return nil
	}
	background.Go(func(ctx context.Context) {
		onboardCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Gateway.PromptTimeout))
		defer cancel()
		if err := onboard.Start(onboardCtx, onboard.Request{
			Owner:   cfg.Feishu.OwnerOpenID,
			Home:    cfg.Gateway.HomePath,
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
			log.Printf("steve: onboard: %v", err)
		}
	})

	log.Printf("steve: starting Feishu long connection")
	if err := channel.Start(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		log.Printf("steve: Feishu connection failed; Console remains available: %v", err)
		view.Observe("channel.error", "feishu", "Feishu connection failed; check channel credentials and restart the Hub")
		channelSettings.SetRuntimeError("Feishu connection failed; check the channel configuration and restart the Hub")
		<-ctx.Done()
	}
	return nil
}
