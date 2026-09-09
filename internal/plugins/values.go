package plugins

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func packagePath(name string) bool {
	if len(name) > 1024 || strings.Count(name, "/") > 32 || name == "" || name == "." || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || strings.HasPrefix(part, ".") || strings.Contains(part, ":") {
			return false
		}
	}
	return true
}

func (m Manifest) validateValue(v Value) error {
	if strings.ContainsRune(v.Text, '\x00') || strings.ContainsRune(v.Prefix, '\x00') {
		return fmt.Errorf("%w: NUL in setting value", ErrInvalid)
	}
	if v.Config == "" && v.Secret == "" {
		if v.Prefix != "" {
			return fmt.Errorf("%w: prefix requires a setting reference", ErrInvalid)
		}
		return nil
	}
	if v.Text != "" || v.Config != "" && v.Secret != "" {
		return fmt.Errorf("%w: value must have exactly one source", ErrInvalid)
	}
	key := v.Config
	if v.Secret != "" {
		key = v.Secret
	}
	setting, ok := m.Settings[key]
	if !ok || setting.Secret != (v.Secret != "") {
		return fmt.Errorf("%w: setting reference %q has no matching declaration", ErrInvalid, key)
	}
	return nil
}

func (m Manifest) validateMCP(server MCPServer) error {
	switch server.Transport {
	case "http", "sse":
		if server.Program != nil || len(server.Env) > 0 || server.URL.Secret != "" || server.URL.Prefix != "" {
			return fmt.Errorf("%w: remote MCP needs a public URL and no program or environment", ErrInvalid)
		}
		if err := m.validateValue(server.URL); err != nil {
			return err
		}
		if server.URL.Config == "" {
			if err := validateEndpoint(server.URL.Text); err != nil {
				return err
			}
		}
		for _, name := range sortedKeys(server.Headers) {
			if !validHeader(name) {
				return fmt.Errorf("%w: invalid MCP header name", ErrInvalid)
			}
			v := server.Headers[name]
			if err := m.validateValue(v); err != nil {
				return err
			}
			if strings.ContainsAny(v.Text+v.Prefix, "\r\n") {
				return fmt.Errorf("%w: newline in MCP header", ErrInvalid)
			}
		}
	case "stdio":
		if server.Program == nil || !packagePath(server.Program.Path) || server.URL != (Value{}) || len(server.Headers) > 0 {
			return fmt.Errorf("%w: stdio MCP needs package code and no URL or headers", ErrInvalid)
		}
		if server.Program.Runtime != "" && server.Program.Runtime != "node" && server.Program.Runtime != "python3" {
			return fmt.Errorf("%w: unsupported MCP runtime", ErrInvalid)
		}
		for _, v := range server.Program.Args {
			if err := m.validateValue(v); err != nil {
				return err
			}
		}
		for _, name := range sortedKeys(server.Env) {
			if !validEnv(name) {
				return fmt.Errorf("%w: invalid environment name", ErrInvalid)
			}
			if err := m.validateValue(server.Env[name]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: MCP transport %q", ErrIncompatible, server.Transport)
	}
	return nil
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return fmt.Errorf("%w: MCP URL must be http(s) without credentials or fragment", ErrInvalid)
	}
	return nil
}

func validHeader(name string) bool {
	if name == "" || http.CanonicalHeaderKey(name) != name {
		return false
	}
	for _, c := range name {
		if c <= 32 || c >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", c) {
			return false
		}
	}
	return true
}

func validEnv(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
