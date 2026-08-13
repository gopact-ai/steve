package setup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/home"
	"golang.org/x/term"
)

type Flags struct {
	ConfigPath       string
	AppID            string
	SecretEnv        string
	Domain           string
	DMPolicy         string
	AllowedSender    string
	GroupPolicy      string
	AllowUnmentioned bool
	CreateApp        bool
}

type Options struct {
	In          io.Reader
	Out         io.Writer
	Env         func(string) string
	Interactive bool
	ReadSecret  func() (string, error)
	Register    func(context.Context, feishu.RegisterOptions) (feishu.CreatedApp, error)
	Probe       func(context.Context, string, string, string) (feishu.Identity, error)
	Owner       func(context.Context, string, string, string) (string, error)
}

func Run(ctx context.Context, flags Flags, opts Options) (*config.Config, error) {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Probe == nil {
		opts.Probe = feishu.Probe
	}
	if opts.Owner == nil {
		opts.Owner = feishu.OwnerOpenID
	}
	if opts.Register == nil {
		opts.Register = feishu.RegisterApp
	}
	reader := bufio.NewReader(opts.In)
	feishuCfg, identity, err := collect(ctx, flags, opts, reader)
	if err != nil {
		return nil, err
	}
	cfg := config.StarterFeishu(feishuCfg)
	if err := cfg.Feishu.Validate(); err != nil {
		return nil, err
	}
	if err := config.Save(flags.ConfigPath, cfg); err != nil {
		return nil, err
	}
	fmt.Fprintf(opts.Out, "steve: created %s\n", flags.ConfigPath)
	if identity.Name != "" || identity.OpenID != "" {
		fmt.Fprintf(opts.Out, "steve: feishu bot %s %s\n", strings.TrimSpace(identity.Name), identity.OpenID)
	}
	if feishuCfg.DMPolicy == config.DMPolicyPairing {
		fmt.Fprintln(opts.Out, "steve: dm policy is pairing; approve senders with: steve pairing approve <CODE>")
	}
	homePath := home.DefaultPath()
	if err := home.Bootstrap(homePath, cfg.Feishu.OwnerOpenID); err != nil {
		return nil, err
	}
	if opts.Interactive {
		if err := maybeFillUser(reader, opts, homePath); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(opts.Out, "steve: home %s\n", homePath)
	fmt.Fprintln(opts.Out, "steve: 编辑 SOUL.md（你是谁）和 USER.md（主人是谁），然后 steve run")
	fmt.Fprintln(opts.Out, "steve: /new 会新开会话，但保留身份和记忆")
	return cfg, nil
}

const (
	methodCreate = "create"
	methodManual = "manual"
)

func collect(ctx context.Context, flags Flags, opts Options, reader *bufio.Reader) (config.Feishu, feishu.Identity, error) {
	appID := strings.TrimSpace(firstNonEmpty(flags.AppID, opts.Env("FEISHU_APP_ID")))
	secret := strings.TrimSpace(opts.Env(secretEnv(flags)))
	domain := strings.TrimSpace(flags.Domain)
	if flags.CreateApp && (appID != "" || secret != "") {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: -create-app cannot be used with -app-id or %s", secretEnv(flags))
	}

	createApp := flags.CreateApp
	if appID == "" && secret == "" && !flags.CreateApp {
		if opts.Interactive {
			method, err := promptSelect(opts, reader, opts.Out, "飞书应用来源", []option{
				{methodCreate, "打开飞书链接 / 扫码创建"},
				{methodManual, "手动粘贴 App ID 和 Secret"},
			}, methodCreate)
			if err != nil {
				return config.Feishu{}, feishu.Identity{}, err
			}
			createApp = method == methodCreate
		} else {
			return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: -app-id is required (or pass -create-app)")
		}
	}

	var scannedOpenID string
	if createApp {
		created, err := opts.Register(ctx, feishu.RegisterOptions{Out: opts.Out, Domain: domain})
		if err != nil {
			if !opts.Interactive {
				return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: %w", err)
			}
			fmt.Fprintf(opts.Out, "steve: create-app failed: %v\n", err)
			appID, secret, domain, err = promptManualCredentials(reader, opts, flags, appID, secret, domain)
			if err != nil {
				return config.Feishu{}, feishu.Identity{}, err
			}
		} else {
			appID = created.AppID
			secret = created.AppSecret
			if created.Domain != "" {
				domain = created.Domain
			}
			scannedOpenID = created.OpenID
			fmt.Fprintf(opts.Out, "steve: created feishu app %s\n", created.AppID)
		}
	}

	if appID == "" {
		if !opts.Interactive {
			return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: -app-id is required")
		}
		var err error
		appID, secret, domain, err = promptManualCredentials(reader, opts, flags, appID, secret, domain)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if secret == "" {
		if !opts.Interactive {
			return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: environment variable %s is empty", secretEnv(flags))
		}
		var err error
		secret, err = promptSecret(opts)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if secret == "" {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: app secret is empty")
	}
	if domain == "" && opts.Interactive && scannedOpenID == "" {
		var err error
		domain, err = promptSelect(opts, reader, opts.Out, "API 域名", []option{
			{config.DomainFeishu, "飞书 open.feishu.cn"},
			{config.DomainLark, "Lark open.larksuite.com"},
		}, config.DomainFeishu)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if domain == "" {
		domain = config.DomainFeishu
	}

	identity, err := opts.Probe(ctx, appID, secret, domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: probe feishu: %w", err)
	}

	owner := scannedOpenID
	if owner == "" {
		resolved, ownerErr := opts.Owner(ctx, appID, secret, domain)
		if ownerErr != nil {
			fmt.Fprintf(opts.Out, "steve: could not resolve app owner: %v\n", ownerErr)
		} else {
			owner = resolved
		}
	}

	dmPolicy := strings.TrimSpace(flags.DMPolicy)
	if flags.AllowedSender != "" && dmPolicy == "" {
		dmPolicy = config.DMPolicyAllowlist
	}
	if dmPolicy == "" && opts.Interactive {
		dmPolicy, err = promptSelect(opts, reader, opts.Out, "私聊策略", []option{
			{config.DMPolicyPairing, "配对码（陌生人私聊先批准）"},
			{config.DMPolicyAllowlist, "仅白名单"},
		}, config.DMPolicyPairing)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if dmPolicy == "" {
		dmPolicy = config.DMPolicyPairing
	}

	senders := splitSenders(flags.AllowedSender)
	if owner != "" && !contains(senders, owner) && (dmPolicy == config.DMPolicyPairing || len(senders) == 0) {
		senders = append([]string{owner}, senders...)
	}
	if dmPolicy == config.DMPolicyAllowlist && len(senders) == 0 && opts.Interactive {
		initial := owner
		line, err := promptText(reader, opts.Out, "Allowed sender open_id", initial)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
		senders = splitSenders(line)
	}

	groupPolicy := strings.TrimSpace(flags.GroupPolicy)
	if groupPolicy == "" && opts.Interactive {
		groupPolicy, err = promptSelect(opts, reader, opts.Out, "群聊策略", []option{
			{config.GroupPolicyAllowlist, "仅白名单用户，且需要 @机器人"},
			{config.GroupPolicyOpen, "群内任何人，且需要 @机器人"},
			{config.GroupPolicyDisabled, "忽略群消息"},
		}, config.GroupPolicyAllowlist)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if groupPolicy == "" {
		groupPolicy = config.GroupPolicyAllowlist
	}

	return config.Feishu{
		AppID:            appID,
		AppSecret:        secret,
		Domain:           domain,
		DMPolicy:         dmPolicy,
		AllowedSenders:   senders,
		GroupPolicy:      groupPolicy,
		AllowUnmentioned: flags.AllowUnmentioned,
		OwnerOpenID:      owner,
	}, identity, nil
}

type option struct {
	value string
	label string
}

func promptManualCredentials(reader *bufio.Reader, opts Options, flags Flags, appID, secret, domain string) (string, string, string, error) {
	var err error
	if appID == "" {
		appID, err = promptText(reader, opts.Out, "Feishu App ID", "")
		if err != nil {
			return "", "", "", err
		}
	}
	if secret == "" {
		secret, err = promptSecret(opts)
		if err != nil {
			return "", "", "", err
		}
	}
	if domain == "" && opts.Interactive {
		domain, err = promptSelect(opts, reader, opts.Out, "API 域名", []option{
			{config.DomainFeishu, "飞书 open.feishu.cn"},
			{config.DomainLark, "Lark open.larksuite.com"},
		}, config.DomainFeishu)
		if err != nil {
			return "", "", "", err
		}
	}
	return appID, secret, domain, nil
}

func promptSecret(opts Options) (string, error) {
	fmt.Fprintf(opts.Out, "App Secret (input hidden): ")
	var secret string
	var err error
	if opts.ReadSecret != nil {
		secret, err = opts.ReadSecret()
	} else {
		secret, err = readSecret()
	}
	fmt.Fprintln(opts.Out)
	if err != nil {
		return "", fmt.Errorf("setup: read app secret: %w", err)
	}
	return strings.TrimSpace(secret), nil
}

func maybeFillUser(reader *bufio.Reader, opts Options, homePath string) error {
	userPath := filepath.Join(homePath, home.FileUser)
	raw, err := os.ReadFile(userPath)
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), home.TemplateMarker) {
		return nil
	}
	name, err := promptOptional(reader, opts.Out, "怎么称呼你？", "")
	if err != nil {
		return err
	}
	tz, err := promptOptional(reader, opts.Out, "时区", "Asia/Shanghai")
	if err != nil {
		return err
	}
	if name == "" && tz == "" {
		return nil
	}
	text := string(raw)
	if name != "" {
		text = replaceLabeled(text, "称呼", name)
	}
	if tz != "" {
		text = replaceLabeled(text, "时区", tz)
	}
	text = strings.Replace(text, home.TemplateMarker+"\n", "", 1)
	return os.WriteFile(userPath, []byte(text), 0o600)
}

func promptOptional(in *bufio.Reader, out io.Writer, question, hint string) (string, error) {
	if hint != "" {
		fmt.Fprintf(out, "%s [%s]: ", question, hint)
	} else {
		fmt.Fprintf(out, "%s: ", question)
	}
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func replaceLabeled(text, label, value string) string {
	needle := "- " + label + "："
	idx := strings.Index(text, needle)
	if idx < 0 {
		return text
	}
	rest := text[idx+len(needle):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		return text[:idx] + needle + value
	}
	return text[:idx] + needle + value + rest[end:]
}

func promptText(in *bufio.Reader, out io.Writer, question, initial string) (string, error) {
	if initial != "" {
		fmt.Fprintf(out, "%s [%s]: ", question, initial)
	} else {
		fmt.Fprintf(out, "%s: ", question)
	}
	line, err := in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = initial
	}
	if line == "" {
		return "", fmt.Errorf("setup: %s is required", strings.ToLower(question))
	}
	return line, nil
}

func readSecret() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

func secretEnv(flags Flags) string {
	if flags.SecretEnv != "" {
		return flags.SecretEnv
	}
	return "FEISHU_APP_SECRET"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func splitSenders(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
