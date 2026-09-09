// Package onboard starts identity setup through the bound channel.
package onboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

type TurnRequest struct {
	ConversationID string
	Input          string
	SenderOpenID   string
	ChatType       string
}

type TurnResult struct {
	Text string
}

type Request struct {
	Owner   string
	Home    string
	Reader  home.Loader
	Store   *state.Store
	Handle  func(context.Context, TurnRequest) (TurnResult, error)
	Send    func(context.Context, string, string) (chatID string, err error)
	Catalog i18n.Catalog
}

// PendingPrefix marks the synthetic conversation an onboarding turn runs
// under before it is relocated into the real home chat.
const PendingPrefix = "steve:onboard:"

func PendingID(owner string) string {
	return PendingPrefix + owner
}

// initReporter is a home reader that knows itself whether the identity
// still needs setting up. The shared (ledger-backed) reader is one; a
// plain directory is judged from what it loads instead.
type initReporter interface {
	NeedsInit() (bool, error)
}

// Bind the shared reader, so it losing NeedsInit fails the build here
// rather than sending onboarding down the directory branch — which reads
// a path the shared identity does not live in. home.Dir is deliberately
// not bound: it is the reader the directory branch is for.
var _ initReporter = home.EditableReader{}

func Start(ctx context.Context, req Request) error {
	if req.Owner == "" || req.Store == nil {
		return nil
	}
	if req.Store.Onboarded() {
		return nil
	}
	needsInit := false
	shared := false
	if reader, ok := req.Reader.(initReporter); ok {
		var err error
		needsInit, err = reader.NeedsInit()
		if err != nil {
			return err
		}
		shared = true
	} else if req.Reader != nil {
		snapshot, err := req.Reader.Load(home.ModeOwner)
		if err != nil && !errors.Is(err, home.ErrMissing) {
			return err
		}
		needsInit = errors.Is(err, home.ErrMissing) || home.IsTemplate(snapshot.Soul) || home.IsTemplate(snapshot.User)
		_, local := req.Reader.(home.Dir)
		shared = !local
	} else {
		needsInit = home.NeedsInit(req.Home)
	}
	if !needsInit {
		return nil
	}
	if req.Handle == nil || req.Send == nil {
		return fmt.Errorf("onboard: handle and send are required")
	}
	pendingID := PendingID(req.Owner)
	prompt := Prompt(req.Catalog.Locale(), req.Home)
	if shared {
		prompt = sharedPrompt(req.Catalog.Locale())
	}
	result, err := req.Handle(ctx, TurnRequest{
		ConversationID: pendingID,
		Input:          prompt,
		SenderOpenID:   req.Owner,
		ChatType:       "p2p",
	})
	if err != nil {
		return err
	}
	text := result.Text
	if text == "" {
		text = req.Catalog.T(i18n.EmptyReply)
	}
	chatID, err := req.Send(ctx, req.Owner, text)
	if err != nil {
		return err
	}
	if err := req.Store.MarkOnboarded(); err != nil {
		return err
	}
	if chatID != "" {
		if err := req.Store.Relocate(pendingID, chatID); err != nil {
			return fmt.Errorf("onboard: relocate session: %w", err)
		}
	}
	return nil
}

func sharedPrompt(locale i18n.Locale) string {
	if locale == i18n.LocaleEN {
		return "You are Steve. This is your first private message to the owner. Briefly introduce yourself and invite them to start with a question or task. They may share their preferred name, timezone, or working preferences when useful; profile setup is optional. Build the profile only from what they choose to tell you. Do not scan local sessions, write profile files, or use tools in this turn."
	}
	return "你是 Steve。这是你第一次私聊联系用户。简短介绍自己，邀请用户直接提出问题或任务。用户可以在需要时告诉你称呼、时区或协作偏好，完善档案是可选的。仅根据用户主动提供的信息完善档案。这一轮不要扫描本机会话、写档案或使用工具。"
}

func Prompt(locale i18n.Locale, homePath string) string {
	if locale == i18n.LocaleEN {
		return "You are Steve. This is the first private Feishu message to your owner.\n" +
			"Your working directory is the Steve home at " + homePath + " (SOUL.md, USER.md, MEMORY.md; they are still templates).\n" +
			"Introduce yourself briefly and invite them to start with a question or task. They may share their preferred name, timezone, or working preferences when useful; profile setup is optional.\n" +
			"Build the profile only from what they choose to tell you. Do not scan local sessions, write the files, or use tools in this turn. Start the conversation now."
	}
	return "你是 Steve。这是你第一次通过飞书私聊联系用户。\n" +
		"当前工作目录是 Steve home：" + homePath + "（SOUL.md / USER.md / MEMORY.md，现在还是模板）。\n" +
		"先简短自我介绍，邀请用户直接提出问题或任务。用户可以在需要时告诉你称呼、时区或协作偏好，完善档案是可选的。\n" +
		"仅根据用户主动提供的信息完善档案。这一轮不要扫描本机会话、写档案或使用工具。现在开始对话。"
}
