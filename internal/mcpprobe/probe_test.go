package mcpprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer answers initialize and tools/list; one tool, named by tag.
func handle(tag string, req rpcRequest) any {
	switch req.Method {
	case "initialize":
		return map[string]any{"protocolVersion": protocolVersion, "serverInfo": map[string]any{"name": "fake-" + tag, "version": "0.1"}, "capabilities": map[string]any{}}
	case "tools/list":
		return map[string]any{"tools": []map[string]any{{"name": "echo_" + tag, "description": "echoes", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}}
	}
	return nil
}

const pythonServer = `
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    req = json.loads(line)
    if "id" not in req: continue
    if req["method"] == "initialize":
        result = {"protocolVersion": "2025-06-18", "serverInfo": {"name": "fake-stdio", "version": "0.1"}, "capabilities": {}}
    elif req["method"] == "tools/list":
        result = {"tools": [{"name": "echo_stdio", "description": "echoes", "inputSchema": {"type": "object"}}]}
    else:
        result = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": result}) + "\n"); sys.stdout.flush()
`

func TestProbeStdio(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	script := filepath.Join(t.TempDir(), "server.py")
	_ = os.WriteFile(script, []byte(pythonServer), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := Probe(ctx, Server{Type: "stdio", Command: "python3", Args: []string{script}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerName != "fake-stdio" || len(r.Tools) != 1 || r.Tools[0].Name != "echo_stdio" || r.Digest == "" {
		t.Fatalf("result = %+v", r)
	}
	_, err = Probe(ctx, Server{Type: "stdio", Command: "false"})
	if err == nil {
		t.Fatal("a server that exits at once probed fine")
	}
}

func TestProbeStreamableHTTPAndSSE(t *testing.T) {
	// Streamable HTTP: JSON replies, a session id, and one reply as an event stream.
	streamable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "s1")
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := handle("http", req)
		if req.Method == "tools/list" {
			w.Header().Set("Content-Type", "text/event-stream")
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}))
	defer streamable.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := Probe(ctx, Server{Type: "http", URL: streamable.URL, Headers: map[string]string{"Authorization": "Bearer x"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerName != "fake-http" || len(r.Tools) != 1 || r.Tools[0].Name != "echo_http" {
		t.Fatalf("http result = %+v", r)
	}
	// Legacy SSE: GET holds the stream, the first event names the POST endpoint.
	replies := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?session=1\n\n")
		fl.Flush()
		for {
			select {
			case msg := <-replies:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusAccepted)
		if req.ID == nil {
			return
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": handle("sse", req)})
		replies <- string(raw)
	})
	legacy := httptest.NewServer(mux)
	defer legacy.Close()
	r, err = Probe(ctx, Server{Type: "sse", URL: legacy.URL + "/sse"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerName != "fake-sse" || len(r.Tools) != 1 || !strings.HasPrefix(r.Tools[0].Name, "echo_sse") {
		t.Fatalf("sse result = %+v", r)
	}
	if DigestOf(r.Tools) == "" || DigestOf(nil) == DigestOf(r.Tools) {
		t.Fatal("digest does not tell sets apart")
	}
}
