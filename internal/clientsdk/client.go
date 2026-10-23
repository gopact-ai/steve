// Package clientsdk is a Go client for steve.client.v1, the protocol
// docs/protocol.md describes. It is typed on internal/clientapi, the
// server's own wire types, and keeps no state of its own beyond the
// event stream's resume cursor: the server is the only state holder.
package clientsdk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/clientapi"
)

// maxBody bounds a JSON response. The largest is a thread page of 500
// items, well under this.
const maxBody = 32 << 20

// maxRefusal bounds the body read from a refusal.
const maxRefusal = 64 << 10

// Client calls one Steve server. It is safe for concurrent use.
type Client struct {
	base  string
	token string
	http  *http.Client
	// Language is sent as Accept-Language when set.
	Language string
}

// New returns a client for the server at baseURL (scheme, host and port,
// without /v1), authenticating with the owner token. A nil httpClient uses
// one that does not follow redirects to another origin.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// BaseURL is the server this client calls.
func (c *Client) BaseURL() string { return c.base }

// Error is a refusal the server answered: the v1 error body, or for 401 and
// other non-JSON answers only the status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("steve: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("steve: %s: %s", e.Code, e.Message)
}

// ErrNotV1 is returned when the server answered with something other than
// the protocol, such as an older build serving its web page for /v1 paths.
var ErrNotV1 = errors.New("the server does not speak steve.client.v1")

// IsCode reports whether err is a server refusal with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// NewCommandID returns a fresh idempotency key. A caller retrying a
// request it is unsure about sends the same key again.
func NewCommandID(prefix string) string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	return prefix + hex.EncodeToString(raw[:])
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.Language != "" {
		req.Header.Set("Accept-Language", c.Language)
	}
	return req, nil
}

// call sends one request and decodes the JSON answer into out.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// A refusal is a short v1 error body; anything longer is not one.
		raw, _ := io.ReadAll(io.LimitReader(res.Body, maxRefusal))
		return refusal(res, raw)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return err
	}
	if !isJSON(res) {
		return ErrNotV1
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func isJSON(res *http.Response) bool {
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	return media == "application/json"
}

func refusal(res *http.Response, raw []byte) error {
	var body clientapi.Error
	if isJSON(res) && json.Unmarshal(raw, &body) == nil && body.Error.Code != "" {
		return &Error{Status: res.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return fmt.Errorf("%w (HTTP %d)", ErrNotV1, res.StatusCode)
	}
	return &Error{Status: res.StatusCode, Message: plainMessage(raw)}
}

// maxPlainMessage bounds the message kept from a refusal that is not a v1
// error body, such as a proxy's HTML page.
const maxPlainMessage = 4 << 10

// plainMessage is a non-protocol error body as one line of at most
// maxPlainMessage bytes, cut on a character boundary.
func plainMessage(raw []byte) string {
	if len(raw) > 4*maxPlainMessage {
		raw = raw[:4*maxPlainMessage]
	}
	s := strings.Join(strings.Fields(strings.ToValidUTF8(string(raw), "")), " ")
	if len(s) <= maxPlainMessage {
		return s
	}
	cut := maxPlainMessage - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// Initialize reads the protocol, server version and capabilities. It
// fails with ErrNotV1 when the server answers another protocol.
func (c *Client) Initialize(ctx context.Context) (clientapi.Initialize, error) {
	var out clientapi.Initialize
	if err := c.call(ctx, http.MethodGet, "/v1/initialize", nil, &out); err != nil {
		return out, err
	}
	if out.Protocol != clientapi.Protocol {
		return out, fmt.Errorf("%w: it answered %q", ErrNotV1, out.Protocol)
	}
	return out, nil
}

// Threads lists every thread, running first, then most recent.
func (c *Client) Threads(ctx context.Context) ([]clientapi.Thread, error) {
	var out clientapi.ThreadList
	err := c.call(ctx, http.MethodGet, "/v1/threads", nil, &out)
	return out.Threads, err
}

// Page selects a page of a thread's items. A zero Limit is the server's
// default; Cursor is a NextCursor already received.
type Page struct {
	Limit  int
	Cursor string
}

// Thread reads a thread, its live turns and its newest page of items.
func (c *Client) Thread(ctx context.Context, id string, page Page) (clientapi.ThreadDetail, error) {
	q := url.Values{}
	if page.Limit > 0 {
		q.Set("limit", strconv.Itoa(page.Limit))
	}
	if page.Cursor != "" {
		q.Set("cursor", page.Cursor)
	}
	path := "/v1/threads/" + url.PathEscape(id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out clientapi.ThreadDetail
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// CreateThread creates a thread, or returns it unchanged when the same ID
// and project are named again.
func (c *Client) CreateThread(ctx context.Context, req clientapi.CreateThread) (clientapi.ThreadDetail, error) {
	var out clientapi.ThreadDetail
	err := c.call(ctx, http.MethodPost, "/v1/threads", req, &out)
	return out, err
}

// StartTurn submits input. Sending the same CommandID and content again
// returns the original turn.
func (c *Client) StartTurn(ctx context.Context, req clientapi.StartTurn) (clientapi.Turn, error) {
	var out clientapi.Turn
	err := c.call(ctx, http.MethodPost, "/v1/turns", req, &out)
	return out, err
}

// Steer starts a queued turn now, interrupting the running one.
func (c *Client) Steer(ctx context.Context, turn string) (clientapi.Turn, error) {
	var out clientapi.Turn
	err := c.call(ctx, http.MethodPost, "/v1/turns/"+url.PathEscape(turn)+"/steer", struct{}{}, &out)
	return out, err
}

// Interrupt removes a queued turn or stops a running one. Retrying with
// the same commandID returns the same control.
func (c *Client) Interrupt(ctx context.Context, turn, commandID string) (clientapi.InterruptResult, error) {
	var out clientapi.InterruptResult
	err := c.call(ctx, http.MethodPost, "/v1/turns/"+url.PathEscape(turn)+"/interrupt", clientapi.Interrupt{CommandID: commandID}, &out)
	return out, err
}

// Answer decides a pending question. Repeating the same answer returns the
// same receipt.
func (c *Client) Answer(ctx context.Context, question string, answer clientapi.QuestionAnswer) (clientapi.AnswerResult, error) {
	var out clientapi.AnswerResult
	err := c.call(ctx, http.MethodPost, "/v1/questions/"+url.PathEscape(question)+"/answer", answer, &out)
	return out, err
}

// AttemptDiff lists the files an attempt changed and, with a path, that
// file's unified diff.
func (c *Client) AttemptDiff(ctx context.Context, attempt, path string) (clientapi.AttemptDiff, error) {
	p := "/v1/attempts/" + url.PathEscape(attempt) + "/diff"
	if path != "" {
		p += "?" + url.Values{"path": {path}}.Encode()
	}
	var out clientapi.AttemptDiff
	err := c.call(ctx, http.MethodGet, p, nil, &out)
	return out, err
}

// Projects lists the declared projects.
func (c *Client) Projects(ctx context.Context) ([]clientapi.Project, error) {
	var out clientapi.ProjectList
	err := c.call(ctx, http.MethodGet, "/v1/projects", nil, &out)
	return out.Projects, err
}

// Agents lists the registered agents.
func (c *Client) Agents(ctx context.Context) ([]clientapi.Agent, error) {
	var out clientapi.AgentList
	err := c.call(ctx, http.MethodGet, "/v1/agents", nil, &out)
	return out.Agents, err
}

// TaskQuery scopes and pages task history. Thread and Project are
// exclusive; Status is all, live or closed.
type TaskQuery struct {
	Thread, Project, Status, Cursor string
	Limit                           int
}

// Tasks reads one page of task history.
func (c *Client) Tasks(ctx context.Context, query TaskQuery) (clientapi.TaskPage, error) {
	q := url.Values{}
	for key, value := range map[string]string{"thread": query.Thread, "project": query.Project, "status": query.Status, "cursor": query.Cursor} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}
	path := "/v1/tasks"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out clientapi.TaskPage
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
