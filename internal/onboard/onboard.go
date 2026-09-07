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

func Start(ctx context.Context, req Request) error {
	if req.Owner == "" || req.Store == nil {
		return nil
	}
	if req.Store.Onboarded() {
		return nil
	}
	needsInit := false
	shared := false
	if reader, ok := req.Reader.(interface{ NeedsInit() (bool, error) }); ok {
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
		return "You are Steve. This is your first private message to the owner. Your shared identity is still a template. Briefly introduce yourself, ask what to call them and confirm their timezone. Build the profile from what they choose to tell you. Do not scan local sessions or use tools in this turn."
	}
	return "你是 Steve。这是你第一次私聊联系用户，共享档案还没有完成。简短介绍自己，问怎么称呼用户，并确认时区。根据用户主动提供的信息构建档案。这一轮不要扫描本机会话，不要使用工具。"
}

func Prompt(locale i18n.Locale, homePath string) string {
	if locale == i18n.LocaleEN {
		return "You are Steve. This is the first private Feishu message to your owner.\n" +
			"Your working directory is the Steve home at " + homePath + " (SOUL.md, USER.md, MEMORY.md; they are still templates).\n" +
			"Introduce yourself briefly. Ask what to call them and confirm timezone.\n" +
			"Tell them you will scan local Codex / Claude / Cursor / Grok / Kimi sessions on this machine to draft their profile, unless they say not to.\n" +
			"Do not write the files in this turn. Do not use tools. Start the conversation now."
	}
	return "你是 Steve。这是你第一次通过飞书私聊联系用户。\n" +
		"当前工作目录是 Steve home：" + homePath + "（SOUL.md / USER.md / MEMORY.md，现在还是模板）。\n" +
		"先简短自我介绍，问怎么称呼，并确认时区。\n" +
		"明确告诉用户：除非他们说不要，你会扫描这台机器上已有的 Codex / Claude / Cursor / Grok / Kimi 会话来构建 USER.md 画像。\n" +
		"这一轮不要写文件，不要用工具。现在开始对话。"
}
