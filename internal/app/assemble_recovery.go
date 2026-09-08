package app

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

func assembleRecovery(input inputAssembly, boot runtimeAssembly, storage ledgerAssembly, work executionAssembly, page consoleAssembly) error {
	environment := input.Environment()
	book := boot.Book()
	ctx := boot.Context()
	quarantinedTasks := storage.QuarantinedTasks()
	catalogText := work.CatalogText()
	coordinator := work.Coordinator()
	gw := work.Gateway()
	tasks := work.Tasks()
	cons := page.Console()

	// Pick back up what a dead gateway left mid-turn: close the orphaned
	// attempt, revive the session, and continue through a real message so
	// the resumed turn renders a card like any other turn.
	var revivals []gateway.Revival
	var dropped []gateway.Notice
	var pageResumes []task.Task
	coordinator.ResumePlans(ctx)
	for _, interrupted := range tasks.Interrupted() {
		if interrupted.Origin == "plan" {
			continue
		}
		if quarantinedTasks[interrupted.ID] {
			slog.Warn(fmt.Sprintf("steve: task #%s has an unconfirmed previous writer; automatic revival is blocked", interrupted.ID), "task", interrupted.ID)
			continue
		}
		if _, err := tasks.Finish(interrupted.ID, task.OutcomeInterrupted, task.Tokens{}, 0); err != nil {
			slog.Error(fmt.Sprintf("steve: close interrupted attempt #%s: %v", interrupted.ID, err), "task", interrupted.ID)
			continue
		}
		// A paused task's attempt still had to be closed, but resuming it
		// would overrule the user who set it down.
		if interrupted.State == task.StatePaused {
			slog.Info(fmt.Sprintf("steve: task #%s is paused; leaving it set aside", interrupted.ID), "task", interrupted.ID)
			continue
		}
		if console.IsConsole(interrupted.Channel) || interrupted.ChatID == console.ChatID {
			// A task the page was running continues on the page, once its
			// queue is loaded: the gateway cannot reply at a web anchor,
			// and a follow-up that waited must not run ahead of the
			// continuation.
			if time.Since(interrupted.UpdatedAt) > staleTask {
				slog.Warn(fmt.Sprintf("steve: task #%s interrupted long ago; leaving it stopped", interrupted.ID), "task", interrupted.ID)
				cons.Notice(turn.TaskNotice{TaskID: interrupted.ID, ChatID: interrupted.ChatID, MessageID: interrupted.AnchorMessage, Requester: interrupted.Requester, Conversation: interrupted.Channel,
					Text: catalogText.T(i18n.TaskDropped, interrupted.ID, time.Since(interrupted.UpdatedAt).Round(time.Hour))})
				continue
			}
			pageResumes = append(pageResumes, interrupted)
			continue
		}
		if interrupted.AnchorMessage == "" {
			// Nothing to reply to, so nothing can be said: the task is
			// only recoverable through the listing.
			slog.Warn(fmt.Sprintf("steve: task #%s interrupted with no anchor; not resumable", interrupted.ID), "task", interrupted.ID)
			continue
		}
		// A task that stops has to say so. Silence here is the one failure
		// the delivery promise cannot survive: the user asked for an hour of
		// work and would otherwise never learn it ended.
		if time.Since(interrupted.UpdatedAt) > staleTask {
			slog.Warn(fmt.Sprintf("steve: task #%s interrupted long ago; leaving it stopped", interrupted.ID), "task", interrupted.ID)
			dropped = append(dropped, gateway.Notice{
				TaskID: interrupted.ID, MessageID: interrupted.AnchorMessage,
				Requester: interrupted.Requester,
				Text: catalogText.T(i18n.TaskDropped, interrupted.ID,
					time.Since(interrupted.UpdatedAt).Round(time.Hour)),
			})
			continue
		}
		revivals = append(revivals, gateway.Revival{
			TaskID: interrupted.ID, Goal: interrupted.Goal, Member: interrupted.Member,
			ConversationID: interrupted.Channel, ChatID: interrupted.ChatID,
			MessageID: interrupted.AnchorMessage, Requester: interrupted.Requester,
			ChatType: interrupted.ChatType,
			OpenCard: interrupted.OpenCard, Interim: interrupted.Interim,
		})
	}
	if len(revivals) > 0 {
		go gw.Revive(revivals, coordinator.ReviveSession)
	}
	for _, notice := range dropped {
		go gw.Notify(notice)
	}

	// Restored queues may run immediately, so wire their dependencies first.
	if err := cons.Persist(book.Document("console")); err != nil {
		return err
	}
	if environment != nil {
		if err := cons.RecoverChats(ctx, coordinator); err != nil {
			return fmt.Errorf("recover running conversations: %w", err)
		}
	}
	for _, t := range pageResumes {
		if err := cons.Resume(ctx, t.Channel, t.ID, t.Member,
			catalogText.T(i18n.ResumeNotice, t.ID), catalogText.T(i18n.ResumePrompt, t.Goal), coordinator.ReviveSession); err != nil {
			slog.Error(fmt.Sprintf("console: resume task #%s: %v", t.ID, err), "task", t.ID, "conversation", t.Channel)
		}
	}
	if err := cons.Drain(); err != nil {
		return err
	}
	return nil
}

// staleTask is how long an interrupted task may sit before the gateway stops
// trying to continue it. Past a day the chat has moved on, and resuming would
// answer a question nobody is still asking.
const staleTask = 24 * time.Hour
