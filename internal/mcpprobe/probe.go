// Package mcpprobe asks an MCP server what tools it has and nothing else:
// initialize, the initialized notification, tools/list, and goodbye. It
// is the smallest client that answers "what would an agent get from this
// server", used on the machine the server is configured on — a stdio
// server can only be started there — and on the hub for its own. It
// never calls a tool.
package mcpprobe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Server is what to probe: a stdio command with its environment, or an
// http (streamable) / sse (legacy) endpoint with its headers.
type Server struct {
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
}

// Tool is one tool as the server describes it. InputSchema is kept as
// the server sent it, for a page to show; Digest summarises the set.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Result is what a probe found: the server's own name and version from
// initialize, its tools, and a digest of the tool set that changes when
// a tool is added, removed, renamed or reshaped.
type Result struct {
	ServerName    string `json:"server_name,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Tools         []Tool `json:"tools"`
	Digest        string `json:"digest"`
	Elapsed       time.Duration
}

const protocolVersion = "2025-06-18"

// Probe runs the handshake and tools/list against one server within ctx.
func Probe(ctx context.Context, s Server) (Result, error) {
	start := time.Now()
	var t transport
	var err error
	switch s.Type {
	case "stdio":
		t, err = openStdio(ctx, s)
	case "http":
		t = &httpTransport{url: s.URL, headers: s.Headers, client: &http.Client{}}
	case "sse":
		t, err = openSSE(ctx, s)
	default:
		return Result{}, fmt.Errorf("mcp: unknown transport %q", s.Type)
	}
	if err != nil {
		return Result{}, err
	}
	defer t.Close()
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := t.Call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "steve-probe", "version": "1"},
	}, &init); err != nil {
		return Result{}, fmt.Errorf("initialize: %w", err)
	}
	if err := t.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return Result{}, fmt.Errorf("initialized: %w", err)
	}
	var tools []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := t.Call(ctx, "tools/list", params, &page); err != nil {
			return Result{}, fmt.Errorf("tools/list: %w", err)
		}
		for _, x := range page.Tools {
			tools = append(tools, Tool{Name: x.Name, Description: x.Description, InputSchema: x.InputSchema})
		}
		if page.NextCursor == "" || len(page.Tools) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	if tools == nil {
		tools = []Tool{}
	}
	return Result{ServerName: init.ServerInfo.Name, ServerVersion: init.ServerInfo.Version, Protocol: init.ProtocolVersion, Tools: tools, Digest: DigestOf(tools), Elapsed: time.Since(start)}, nil
}

// DigestOf hashes the tool set: names and input schemas, in name order,
// with the schemas re-encoded so key order does not matter.
func DigestOf(tools []Tool) string {
	h := sha256.New()
	sorted := append([]Tool{}, tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, t := range sorted {
		h.Write([]byte(t.Name))
		h.Write([]byte{0})
		var v any
		if len(t.InputSchema) > 0 && json.Unmarshal(t.InputSchema, &v) == nil {
			canon, _ := json.Marshal(v)
			h.Write(canon)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---------------------------------------------------------------- wire

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"`
}

type transport interface {
	Call(ctx context.Context, method string, params any, out any) error
	Notify(ctx context.Context, method string, params any) error
	Close()
}

func decodeResult(resp rpcResponse, out any) error {
	if resp.Error != nil {
		return fmt.Errorf("server error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Result, out)
}

// ---------------------------------------------------------------- stdio

type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	mu     sync.Mutex
	nextID int64
	stderr bytes.Buffer
}

func openStdio(ctx context.Context, s Server) (*stdioTransport, error) {
	if strings.TrimSpace(s.Command) == "" {
		return nil, errors.New("mcp: stdio server has no command")
	}
	cmd := exec.CommandContext(ctx, s.Command, s.Args...)
	env := os.Environ()
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin}
	cmd.Stderr = &limitedWriter{buf: &t.stderr, max: 4096}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.Command, err)
	}
	t.lines = bufio.NewScanner(stdout)
	t.lines.Buffer(make([]byte, 1<<20), 8<<20)
	return t, nil
}

func (t *stdioTransport) send(msg rpcRequest) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = t.stdin.Write(raw)
	return err
}

func (t *stdioTransport) Call(ctx context.Context, method string, params any, out any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	id := t.nextID
	if err := t.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		for t.lines.Scan() {
			line := bytes.TrimSpace(t.lines.Bytes())
			if len(line) == 0 {
				continue
			}
			var resp rpcResponse
			if json.Unmarshal(line, &resp) != nil {
				continue
			}
			var got int64
			if resp.Method != "" || json.Unmarshal(resp.ID, &got) != nil || got != id {
				continue // a notification or someone else's answer
			}
			done <- decodeResult(resp, out)
			return
		}
		if err := t.lines.Err(); err != nil {
			done <- err
			return
		}
		done <- fmt.Errorf("server closed its output%s", t.stderrTail())
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *stdioTransport) Notify(_ context.Context, method string, params any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) Close() {
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
}

func (t *stdioTransport) stderrTail() string {
	s := strings.TrimSpace(t.stderr.String())
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return ": " + s
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() < w.max {
		w.buf.Write(p[:min(len(p), w.max-w.buf.Len())])
	}
	return len(p), nil
}

// ---------------------------------------------------------------- streamable http

type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	session string
	nextID  int64
}

func (t *httpTransport) post(ctx context.Context, msg rpcRequest) (*http.Response, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.session = sid
	}
	return resp, nil
}

func (t *httpTransport) Call(ctx context.Context, method string, params any, out any) error {
	t.nextID++
	id := t.nextID
	resp, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		r, err := readSSEResponse(resp.Body, id)
		if err != nil {
			return err
		}
		return decodeResult(r, out)
	}
	var r rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("bad JSON-RPC reply: %w", err)
	}
	return decodeResult(r, out)
}

func (t *httpTransport) Notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *httpTransport) Close() {
	if t.session == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", t.session)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// readSSEResponse reads an event stream until the reply to id arrives.
func readSSEResponse(body io.Reader, id int64) (rpcResponse, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if line != "" {
			continue
		}
		if len(data) == 0 {
			continue
		}
		var r rpcResponse
		payload := strings.Join(data, "\n")
		data = nil
		if json.Unmarshal([]byte(payload), &r) != nil {
			continue
		}
		var got int64
		if r.Method == "" && json.Unmarshal(r.ID, &got) == nil && got == id {
			return r, nil
		}
	}
	if err := sc.Err(); err != nil {
		return rpcResponse{}, err
	}
	return rpcResponse{}, errors.New("event stream ended before the reply")
}

// ---------------------------------------------------------------- legacy sse

// sseTransport is the older HTTP+SSE transport: a GET holds an event
// stream open, its first event names where to POST, and replies come
// back on the stream.
type sseTransport struct {
	post    string
	headers map[string]string
	client  *http.Client
	events  io.ReadCloser
	replies chan rpcResponse
	nextID  int64
}

func openSSE(ctx context.Context, s Server) (*sseTransport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d opening the event stream", resp.StatusCode)
	}
	t := &sseTransport{headers: s.Headers, client: client, events: resp.Body, replies: make(chan rpcResponse, 16)}
	endpoint := make(chan string, 1)
	go t.read(endpoint)
	select {
	case ep := <-endpoint:
		if ep == "" {
			resp.Body.Close()
			return nil, errors.New("event stream gave no endpoint")
		}
		base, err := req.URL.Parse(ep)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		t.post = base.String()
	case <-ctx.Done():
		resp.Body.Close()
		return nil, ctx.Err()
	}
	return t, nil
}

func (t *sseTransport) read(endpoint chan<- string) {
	defer close(t.replies)
	sc := bufio.NewScanner(t.events)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	event, data := "", []string{}
	sent := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			payload := strings.Join(data, "\n")
			if event == "endpoint" && !sent {
				endpoint <- payload
				sent = true
			} else if payload != "" {
				var r rpcResponse
				if json.Unmarshal([]byte(payload), &r) == nil && r.Method == "" {
					t.replies <- r
				}
			}
			event, data = "", nil
		}
	}
	if !sent {
		endpoint <- ""
	}
}

func (t *sseTransport) send(ctx context.Context, msg rpcRequest) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.post, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d posting %s", resp.StatusCode, msg.Method)
	}
	return nil
}

func (t *sseTransport) Call(ctx context.Context, method string, params any, out any) error {
	t.nextID++
	id := t.nextID
	if err := t.send(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return err
	}
	for {
		select {
		case r, ok := <-t.replies:
			if !ok {
				return errors.New("event stream closed before the reply")
			}
			var got int64
			if json.Unmarshal(r.ID, &got) == nil && got == id {
				return decodeResult(r, out)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *sseTransport) Notify(ctx context.Context, method string, params any) error {
	return t.send(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *sseTransport) Close() { _ = t.events.Close() }
