package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/i18n"
	"golang.org/x/term"
)

type Flags struct {
	ConfigPath       string
	AppID            string
	SecretEnv        string
	Domain           string
	AllowedSender    string
	BlockedSender    string
	GroupPolicy      string
	AllowUnmentioned bool
	CreateApp        bool
	OwnerOpenID      string
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
	Catalog     i18n.Catalog
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
	cached, complete := loadCached(flags.ConfigPath)
	if opts.Catalog.IsZero() {
		locale := i18n.FromLang(opts.Env("LANG"))
		if flags.Domain != "" {
			locale = i18n.FromDomain(flags.Domain)
		} else if complete {
			locale = i18n.FromDomain(cached.Domain)
		}
		opts.Catalog = i18n.New(locale)
	}
	reader := bufio.NewReader(opts.In)
	feishuCfg, identity, err := collect(ctx, flags, opts, reader, cached, complete)
	if err != nil {
		return nil, err
	}
	cfg := config.StarterFeishu(feishuCfg)
	if err := cfg.Feishu.Validate(); err != nil {
		return nil, err
	}
	cfg, err = mergeExisting(flags.ConfigPath, cfg)
	if err != nil {
		return nil, err
	}
	if err := config.Save(flags.ConfigPath, cfg); err != nil {
		return nil, err
	}
	text := i18n.New(i18n.FromDomain(cfg.Feishu.Domain))
	opts.Catalog = text
	fmt.Fprintln(opts.Out, text.T(i18n.SetupCreatedConfig, flags.ConfigPath))
	if identity.Name != "" || identity.OpenID != "" {
		fmt.Fprintln(opts.Out, text.T(i18n.SetupBotIdentity, strings.TrimSpace(identity.Name), identity.OpenID))
	}
	if cfg.Feishu.OwnerOpenID != "" {
		fmt.Fprintln(opts.Out, text.T(i18n.SetupOwnerBound, cfg.Feishu.OwnerOpenID))
	}
	fmt.Fprintln(opts.Out, text.T(i18n.SetupEditHome))
	return cfg, nil
}

func loadCached(path string) (config.Feishu, bool) {
	existing, err := config.Load(path)
	if err != nil {
		return config.Feishu{}, false
	}
	complete := existing.Feishu.AppID != "" && existing.Feishu.AppSecret != ""
	return existing.Feishu, complete
}

func mergeExisting(path string, fresh *config.Config) (*config.Config, error) {
	existing, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fresh, nil
		}
		return nil, err
	}
	existing.Feishu = fresh.Feishu
	if err := existing.Feishu.Validate(); err != nil {
		return nil, err
	}
	mergeMissingAgents(existing, fresh)
	return existing, nil
}

func mergeMissingAgents(existing, fresh *config.Config) {
	if existing.Agents == nil {
		existing.Agents = map[string]config.Agent{}
	}
	if existing.Harnesses == nil {
		existing.Harnesses = map[string]config.Harness{}
	}
	for id, item := range fresh.Agents {
		if _, ok := existing.Agents[id]; !ok {
			existing.Agents[id] = item
		}
	}
	for id, item := range fresh.Harnesses {
		if _, ok := existing.Harnesses[id]; !ok {
			existing.Harnesses[id] = item
		}
	}
}

const (
	methodCreate = "create"
	methodManual = "manual"

	reuseKeep   = "keep"
	reuseCreds  = "creds"
	reuseAccess = "access"
)

func collect(ctx context.Context, flags Flags, opts Options, reader *bufio.Reader, cached config.Feishu, complete bool) (config.Feishu, feishu.Identity, error) {
	appID := strings.TrimSpace(firstNonEmpty(flags.AppID, opts.Env("FEISHU_APP_ID"), cached.AppID))
	secret := strings.TrimSpace(firstNonEmpty(opts.Env(secretEnv(flags)), cached.AppSecret))
	domain := strings.TrimSpace(firstNonEmpty(flags.Domain, cached.Domain))
	if flags.CreateApp && (flags.AppID != "" || opts.Env(secretEnv(flags)) != "") {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: -create-app cannot be used with -app-id or %s", secretEnv(flags))
	}

	if complete && opts.Interactive && !flags.CreateApp && flags.AppID == "" {
		action, err := promptSelect(opts, reader, opts.Out, opts.Catalog.T(i18n.SetupReuseAction), []option{
			{reuseKeep, opts.Catalog.T(i18n.SetupReuseKeep)},
			{reuseCreds, opts.Catalog.T(i18n.SetupReuseCreds)},
			{reuseAccess, opts.Catalog.T(i18n.SetupReuseAccess)},
		}, reuseKeep)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
		switch action {
		case reuseKeep:
			return finishCached(ctx, opts, reader, cached)
		case reuseCreds:
			return collectCredentials(ctx, flags, opts, reader, cached)
		case reuseAccess:
			return collectAccess(ctx, flags, opts, reader, cached)
		}
	}

	createApp := flags.CreateApp
	if appID == "" && secret == "" && !flags.CreateApp {
		if opts.Interactive {
			method, err := promptSelect(opts, reader, opts.Out, opts.Catalog.T(i18n.SetupAppSource), []option{
				{methodCreate, opts.Catalog.T(i18n.SetupAppSourceCreate)},
				{methodManual, opts.Catalog.T(i18n.SetupAppSourceManual)},
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
		created, err := opts.Register(ctx, feishu.RegisterOptions{Out: opts.Out, Domain: domain, Catalog: opts.Catalog})
		if err != nil {
			if !opts.Interactive {
				return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: %w", err)
			}
			fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupCreateAppFailed, err))
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
			fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupCreatedApp, created.AppID))
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
		domain, err = promptSelect(opts, reader, opts.Out, opts.Catalog.T(i18n.SetupDomain), domainOptions(opts.Catalog), config.DomainFeishu)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}
	if domain == "" {
		domain = config.DomainFeishu
	}
	opts.Catalog = i18n.New(i18n.FromDomain(domain))

	identity, err := opts.Probe(ctx, appID, secret, domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: probe feishu: %w", err)
	}

	groupPolicy, senders, blocked, err := collectGroupAccess(flags, opts, reader, cached, false)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, err
	}

	// The owner binds last: whoever it names gets the owner home, and the
	// next steve run opens the home chat with them. The flag wins outright;
	// otherwise auto-resolution only suggests, and the person confirms or
	// replaces the value themselves: their id is theirs to fill in.
	owner := firstNonEmpty(flags.OwnerOpenID, scannedOpenID, cached.OwnerOpenID)
	if owner == "" && flags.OwnerOpenID == "" {
		resolved, ownerErr := opts.Owner(ctx, appID, secret, domain)
		if ownerErr != nil {
			fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupOwnerResolveFail, ownerErr))
		} else {
			owner = resolved
		}
	}
	if opts.Interactive && flags.OwnerOpenID == "" {
		fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupOwnerHint))
		owner, err = promptOptional(reader, opts.Out, opts.Catalog.T(i18n.SetupOwnerPrompt), owner)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
	}

	allowUnmentioned := flags.AllowUnmentioned
	if complete && !flags.AllowUnmentioned {
		allowUnmentioned = cached.AllowUnmentioned
	}
	return config.Feishu{
		AppID:            appID,
		AppSecret:        secret,
		Domain:           domain,
		AllowedSenders:   senders,
		BlockedSenders:   blocked,
		GroupPolicy:      groupPolicy,
		AllowUnmentioned: allowUnmentioned,
		OwnerOpenID:      owner,
	}, identity, nil
}

func finishCached(ctx context.Context, opts Options, reader *bufio.Reader, cached config.Feishu) (config.Feishu, feishu.Identity, error) {
	fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupUsingCached))
	opts.Catalog = i18n.New(i18n.FromDomain(cached.Domain))
	identity, err := opts.Probe(ctx, cached.AppID, cached.AppSecret, cached.Domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: probe feishu: %w", err)
	}
	if cached.OwnerOpenID == "" {
		resolved, ownerErr := opts.Owner(ctx, cached.AppID, cached.AppSecret, cached.Domain)
		if ownerErr != nil {
			fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupOwnerResolveFail, ownerErr))
		} else {
			cached.OwnerOpenID = resolved
		}
	}
	if opts.Interactive {
		fmt.Fprintln(opts.Out, opts.Catalog.T(i18n.SetupOwnerHint))
		owner, err := promptOptional(reader, opts.Out, opts.Catalog.T(i18n.SetupOwnerPrompt), cached.OwnerOpenID)
		if err != nil {
			return config.Feishu{}, feishu.Identity{}, err
		}
		cached.OwnerOpenID = owner
	}
	return cached, identity, nil
}

func collectCredentials(ctx context.Context, flags Flags, opts Options, reader *bufio.Reader, cached config.Feishu) (config.Feishu, feishu.Identity, error) {
	appID, secret, domain, err := promptManualCredentials(reader, opts, flags, cached.AppID, cached.AppSecret, cached.Domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, err
	}
	if domain == "" {
		domain = cached.Domain
	}
	if domain == "" {
		domain = config.DomainFeishu
	}
	opts.Catalog = i18n.New(i18n.FromDomain(domain))
	identity, err := opts.Probe(ctx, appID, secret, domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: probe feishu: %w", err)
	}
	cached.AppID = appID
	cached.AppSecret = secret
	cached.Domain = domain
	return cached, identity, nil
}

func collectAccess(ctx context.Context, flags Flags, opts Options, reader *bufio.Reader, cached config.Feishu) (config.Feishu, feishu.Identity, error) {
	opts.Catalog = i18n.New(i18n.FromDomain(cached.Domain))
	identity, err := opts.Probe(ctx, cached.AppID, cached.AppSecret, cached.Domain)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, fmt.Errorf("setup: probe feishu: %w", err)
	}
	groupPolicy, senders, blocked, err := collectGroupAccess(Flags{}, opts, reader, cached, true)
	if err != nil {
		return config.Feishu{}, feishu.Identity{}, err
	}
	cached.GroupPolicy = groupPolicy
	cached.AllowedSenders = senders
	cached.BlockedSenders = blocked
	cached.DMPolicy = ""
	return cached, identity, nil
}

func collectGroupAccess(flags Flags, opts Options, reader *bufio.Reader, cached config.Feishu, forcePrompt bool) (string, []string, []string, error) {
	groupPolicy := strings.TrimSpace(firstNonEmpty(flags.GroupPolicy, cached.GroupPolicy))
	if opts.Interactive && flags.GroupPolicy == "" && (forcePrompt || groupPolicy == "") {
		def := groupPolicy
		if def == "" {
			def = config.GroupPolicyOpen
		}
		var err error
		groupPolicy, err = promptSelect(opts, reader, opts.Out, opts.Catalog.T(i18n.SetupGroupPolicy), []option{
			{config.GroupPolicyOpen, opts.Catalog.T(i18n.SetupGroupOpen)},
			{config.GroupPolicyAllowlist, opts.Catalog.T(i18n.SetupGroupAllowlist)},
			{config.GroupPolicyDisabled, opts.Catalog.T(i18n.SetupGroupDisabled)},
		}, def)
		if err != nil {
			return "", nil, nil, err
		}
	}
	if groupPolicy == "" {
		groupPolicy = config.GroupPolicyOpen
	}

	senders := splitSenders(flags.AllowedSender)
	if flags.AllowedSender == "" {
		if groupPolicy == config.GroupPolicyOpen {
			senders = nil
		} else {
			senders = append([]string{}, cached.AllowedSenders...)
		}
	}
	if groupPolicy == config.GroupPolicyAllowlist && len(senders) == 0 && opts.Interactive {
		initial := strings.Join(cached.AllowedSenders, ",")
		line, err := promptText(reader, opts.Out, opts.Catalog.T(i18n.SetupAllowedSender), initial)
		if err != nil {
			return "", nil, nil, err
		}
		senders = splitSenders(line)
	}

	blocked := splitSenders(flags.BlockedSender)
	if flags.BlockedSender == "" {
		blocked = append([]string{}, cached.BlockedSenders...)
	}
	return groupPolicy, senders, blocked, nil
}

type option struct {
	value string
	label string
}

func promptManualCredentials(reader *bufio.Reader, opts Options, flags Flags, appID, secret, domain string) (string, string, string, error) {
	var err error
	if opts.Interactive {
		appID, err = promptText(reader, opts.Out, opts.Catalog.T(i18n.SetupAppID), appID)
		if err != nil {
			return "", "", "", err
		}
	} else if appID == "" {
		return "", "", "", fmt.Errorf("setup: -app-id is required")
	}
	if secret == "" {
		secret, err = promptSecret(opts)
		if err != nil {
			return "", "", "", err
		}
	} else if opts.Interactive {
		secret, err = promptSecretKeep(opts, secret)
		if err != nil {
			return "", "", "", err
		}
	}
	if opts.Interactive {
		initial := domain
		if initial == "" {
			initial = config.DomainFeishu
		}
		domain, err = promptSelect(opts, reader, opts.Out, opts.Catalog.T(i18n.SetupDomain), domainOptions(opts.Catalog), initial)
		if err != nil {
			return "", "", "", err
		}
	}
	return appID, secret, domain, nil
}

func promptSecretKeep(opts Options, current string) (string, error) {
	fmt.Fprint(opts.Out, opts.Catalog.T(i18n.SetupAppSecretKeep))
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
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return current, nil
	}
	return secret, nil
}

func promptSecret(opts Options) (string, error) {
	fmt.Fprint(opts.Out, opts.Catalog.T(i18n.SetupAppSecret))
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

// promptOptional asks for a value that may stay empty: enter keeps the
// suggestion (or skips when there is none), a lone "-" clears it.
func promptOptional(in *bufio.Reader, out io.Writer, question, initial string) (string, error) {
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
		return initial, nil
	}
	if line == "-" {
		return "", nil
	}
	return line, nil
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

func domainOptions(text i18n.Catalog) []option {
	return []option{
		{config.DomainFeishu, text.T(i18n.SetupDomainFeishu)},
		{config.DomainLark, text.T(i18n.SetupDomainLark)},
	}
}
