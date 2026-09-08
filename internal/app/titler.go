package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/text"
	"github.com/gopact-ai/steve/internal/view"
)

// conversationTitler names a console conversation the way a chat app
// does: it shows the default agent the first exchange, in a session of
// its own that is closed right after, and asks for a few words that say
// what the owner wants. The conversation's own session is not touched —
// a question about naming has no place in its memory.
type conversationTitler struct {
	manager  *harness.Manager
	catalog  *agent.Catalog
	projects *project.Store
	// home is the hub's own directory to open the session in when the
	// default agent is here; elsewhere any project directory on that
	// machine serves, since the agent is told not to touch it.
	home string
}

func (t *conversationTitler) Title(ctx context.Context, prompt, reply string) (string, error) {
	a := t.catalog.Default()
	if a.ID == "" {
		return "", errors.New("no default agent")
	}
	workdir := t.home
	if a.Node != "" {
		list, err := t.projects.List(ctx)
		if err != nil {
			return "", err
		}
		dir, ok := probeWorkspace(list, a.Node)
		if !ok {
			return "", fmt.Errorf("no directory on %s to open a session in", a.Node)
		}
		workdir = dir
	}
	at := harness.Placement{Node: a.Node, Harness: a.Harness}
	runner, err := t.manager.OpenSession(ctx, at, "", workdir, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = t.manager.CloseSession(context.Background(), at, runner.ID()) }()
	if a.Model != "" || len(a.Options) > 0 {
		harness.ApplyPreferences(ctx, runner, a.ID, a.Model, a.Options)
	}
	answer, _, err := runner.Prompt(ctx, titlePrompt(prompt, reply), func(view.Progress) {})
	if err != nil {
		return "", err
	}
	title := cleanTitle(answer)
	if title == "" {
		return "", fmt.Errorf("agent answered with no title: %q", text.Clip(answer, 80))
	}
	return title, nil
}

// titlePrompt is the question. The exchange is clipped: a title needs
// the gist, not the transcript.
func titlePrompt(prompt, reply string) string {
	return strings.Join([]string{
		"下面是一次对话的开头。请给这次对话起一个标题：概括用户想做的事，不超过 12 个汉字或 6 个英文单词。",
		"只输出标题本身——不要引号、句号、前缀或解释；不要执行任何操作，不要读写任何文件。",
		"",
		"用户：",
		text.Clip(strings.TrimSpace(prompt), 1500),
		"",
		"助手：",
		text.Clip(strings.TrimSpace(reply), 800),
	}, "\n")
}

// cleanTitle takes the title out of whatever the agent wrapped it in: the
// first non-empty line, without a "标题：" prefix, quotes, backticks or
// trailing punctuation.
func cleanTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, prefix := range []string{"标题：", "标题:", "Title:", "title:", "# "} {
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
		line = strings.Trim(line, "\"'`“”‘’《》「」*")
		line = strings.TrimRight(line, "。．.!！?？;；,，")
		if r := []rune(line); len(r) > 32 {
			line = string(r[:32])
		}
		return strings.TrimSpace(line)
	}
	return ""
}
