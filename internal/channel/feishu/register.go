package feishu

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	larkreg "github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	qrcode "github.com/skip2/go-qrcode"
)

type CreatedApp struct {
	AppID     string
	AppSecret string
	Domain    string
	OpenID    string
}

type RegisterOptions struct {
	Out      io.Writer
	Domain   string
	OpenURL  func(string) error
	OnQRCode func(url string, expireIn int)
}

func RegisterApp(ctx context.Context, opts RegisterOptions) (CreatedApp, error) {
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	if opts.OpenURL == nil {
		opts.OpenURL = openBrowser
	}
	accounts := "https://accounts.feishu.cn"
	if opts.Domain == config.DomainLark {
		accounts = "https://accounts.larksuite.com"
	} else if strings.HasPrefix(opts.Domain, "http://") || strings.HasPrefix(opts.Domain, "https://") {
		accounts = opts.Domain
	}

	result, err := larkreg.RegisterApp(ctx, &larkreg.Options{
		Source:     "steve",
		Domain:     accounts,
		LarkDomain: "https://accounts.larksuite.com",
		AppPreset: &larkreg.AppPreset{
			Name: "Steve",
			Desc: "Steve Feishu agent",
		},
		Addons: &larkreg.AppAddons{
			Scopes: larkreg.AppAddonsScopes{
				Tenant: []string{"im:message", "im:message:send_as_bot", "im:message.group_at_msg", "im:message.reactions:write_only"},
			},
			Events: larkreg.AppAddonsEvents{
				Items: larkreg.AppAddonsEventItems{
					Tenant: []string{"im.message.receive_v1"},
				},
			},
		},
		OnQRCode: func(info *larkreg.QRCodeInfo) {
			if opts.OnQRCode != nil {
				opts.OnQRCode(info.URL, info.ExpireIn)
			}
			printRegistrationLink(opts.Out, info.URL, info.ExpireIn)
			if err := opts.OpenURL(info.URL); err != nil {
				fmt.Fprintf(opts.Out, "steve: could not open browser: %v\n", err)
			}
		},
	})
	if err != nil {
		return CreatedApp{}, fmt.Errorf("create feishu app: %w", err)
	}
	if result.ClientID == "" || result.ClientSecret == "" {
		return CreatedApp{}, fmt.Errorf("create feishu app: empty client credentials")
	}
	created := CreatedApp{
		AppID:     result.ClientID,
		AppSecret: result.ClientSecret,
		Domain:    config.DomainFeishu,
	}
	if result.UserInfo != nil {
		created.OpenID = result.UserInfo.OpenID
		if result.UserInfo.TenantBrand == config.DomainLark {
			created.Domain = config.DomainLark
		}
	}
	return created, nil
}

func printRegistrationLink(out io.Writer, rawURL string, expireIn int) {
	mins := expireIn / 60
	if mins < 1 {
		mins = 1
	}
	fmt.Fprintf(out, "\n请用飞书打开下面的链接完成应用创建（也可扫码）：\n\n  %s\n\n", rawURL)
	if qr, err := qrcode.New(rawURL, qrcode.Medium); err == nil {
		fmt.Fprint(out, qr.ToSmallString(false))
	}
	fmt.Fprintf(out, "链接有效期约 %d 分钟。\n\n", mins)
}

func openBrowser(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("refusing to open %s url", parsed.Scheme)
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", parsed.String()).Start()
	case "linux":
		return exec.Command("xdg-open", parsed.String()).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", parsed.String()).Start()
	default:
		return fmt.Errorf("unsupported platform")
	}
}
