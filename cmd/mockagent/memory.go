package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/acp"
)

// Native memory is opt-in and kept only in the explicitly supplied fixture
// directory. Default mockagent sessions retain their original behavior.
type fixtureMemory struct {
	Session string `json:"session"`
	Marker  string `json:"marker,omitempty"`
	Model   string `json:"model"`
	Mode    string `json:"mode"`
}

func memoryPath(id acp.SessionID) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(os.Getenv("MOCKAGENT_MEMORY_DIR"), hex.EncodeToString(hash[:])+".json")
}

func readMemory(id acp.SessionID) (fixtureMemory, error) {
	var memory fixtureMemory
	raw, err := os.ReadFile(memoryPath(id))
	if err != nil {
		return memory, err
	}
	if err := json.Unmarshal(raw, &memory); err != nil {
		return memory, err
	}
	if memory.Session != string(id) {
		return memory, errors.New("fixture native session identity mismatch")
	}
	return memory, nil
}

func writeMemory(memory fixtureMemory) error {
	raw, err := json.Marshal(memory)
	if err != nil {
		return err
	}
	return os.WriteFile(memoryPath(acp.SessionID(memory.Session)), raw, 0o600)
}

func memoryEvent(kind string, id acp.SessionID, input string) error {
	dir := os.Getenv("MOCKAGENT_MEMORY_DIR")
	if dir == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		Kind    string `json:"kind"`
		Session string `json:"session"`
		PID     int    `json:"pid"`
		Input   string `json:"input,omitempty"`
	}{kind, string(id), os.Getpid(), input})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(raw, '\n'))
	return errors.Join(writeErr, file.Close())
}

func newMemorySession() (acp.SessionID, error) {
	dir := os.Getenv("MOCKAGENT_MEMORY_DIR")
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) || !info.IsDir() {
		return "", errors.New("MOCKAGENT_MEMORY_DIR must be an existing absolute fixture directory")
	}
	var random [16]byte
	rand.Read(random[:])
	id := acp.SessionID("mock-memory-" + hex.EncodeToString(random[:]))
	if err := writeMemory(fixtureMemory{Session: string(id), Model: "mock-fast", Mode: "agent"}); err != nil {
		return "", err
	}
	return id, memoryEvent("new", id, "")
}

func (a *agent) memoryPrompt(ctx context.Context, req *acp.PromptRequest, input string) (bool, error) {
	if os.Getenv("MOCKAGENT_MEMORY_DIR") == "" {
		return false, nil
	}
	if err := memoryEvent("prompt", req.SessionID, input); err != nil {
		return true, err
	}
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "fixture-remember" && fields[0] != "fixture-recall") {
			continue
		}
		memory, err := readMemory(req.SessionID)
		if err != nil {
			return true, err
		}
		if fields[0] == "fixture-remember" && len(fields) == 2 {
			memory.Marker = fields[1]
			if err := writeMemory(memory); err != nil {
				return true, err
			}
			return true, a.say(ctx, req.SessionID, "memory: stored")
		}
		if fields[0] == "fixture-recall" && len(fields) == 1 {
			if memory.Marker == "" {
				return true, errors.New("fixture native session has no memory")
			}
			target, missing := a.steveServer(req.SessionID)
			if target == nil {
				return true, errors.New(missing)
			}
			if _, bad, err := a.mcpTool(target, "steve_help", map[string]any{}); err != nil || bad {
				return true, fmt.Errorf("fixture retained MCP rejected: tool_error=%v error=%v", bad, err)
			}
			return true, a.say(ctx, req.SessionID, fmt.Sprintf("memory: %s model=%s mode=%s mcp=ok",
				memory.Marker, chosen(&a.model, "mock-fast"), chosen(&a.mode, "agent")))
		}
		return true, errors.New("invalid fixture memory command")
	}
	return false, nil
}
