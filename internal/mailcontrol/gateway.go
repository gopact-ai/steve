// Package mailcontrol connects owner administration to the external mail
// runtime. The hub holds no provider SDK or mailbox credentials.
package mailcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

type Gateway struct {
	origin string
	token  string
	setup  error
	client *http.Client
}

func FromEnvironment() *Gateway {
	address := os.Getenv("STEVE_MAIL_ADMIN_URL")
	if address == "" {
		address = "http://127.0.0.1:8787"
	}
	return New(address, os.Getenv("STEVE_MAIL_ADMIN_TOKEN"))
}

func New(address, token string) *Gateway {
	g := &Gateway{token: token, client: &http.Client{Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("mail administration redirects are refused")
		}}}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		g.setup = errors.New("set STEVE_MAIL_ADMIN_URL to the mail provider's local 127.0.0.1 port")
	} else if len(token) < 32 {
		g.setup = errors.New("configure STEVE_MAIL_ADMIN_TOKEN and start the mail provider with serve")
	} else {
		g.origin = "http://" + u.Host
	}
	return g
}

var pathPattern = regexp.MustCompile(`^/(accounts(/[A-Za-z0-9_-]{1,128}(/(login|sync))?)?|logins/[A-Za-z0-9_-]{1,128}|imports/[A-Za-z0-9_-]{1,128}|exports|apps|ai)$`)

func (g *Gateway) Exchange(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, int, error) {
	if g.setup != nil {
		return nil, http.StatusServiceUnavailable, g.setup
	}
	if !pathPattern.MatchString(path) || len(body) > 8<<20 {
		return nil, http.StatusBadRequest, errors.New("invalid mail administration request")
	}
	request, err := http.NewRequestWithContext(ctx, method, g.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("invalid mail administration request")
	}
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := g.client.Do(request)
	if err != nil {
		return nil, http.StatusServiceUnavailable, errors.New("mail provider is unavailable; start serve and retry")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 || !json.Valid(raw) {
		return nil, http.StatusBadGateway, errors.New("invalid mail provider response")
	}
	return raw, response.StatusCode, nil
}
