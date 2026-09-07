// Package sshconnect discovers configured SSH destinations and separates
// connection checks, installation previews, and explicit installation.
package sshconnect

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Candidate contains static hints, not a replacement for OpenSSH's resolver.
// Authentication material and executable config directives are never returned.
type Candidate struct {
	Alias           string `json:"alias"`
	HostName        string `json:"host_name"`
	User            string `json:"user,omitempty"`
	Port            int    `json:"port"`
	ProxyJump       string `json:"proxy_jump,omitempty"`
	HasProxyCommand bool   `json:"has_proxy_command"`
	HasIdentityFile bool   `json:"has_identity_file"`
	Conditional     bool   `json:"conditional"`
	Source          string `json:"source"`
	Line            int    `json:"line"`
}

type Warning struct {
	Source  string `json:"source"`
	Line    int    `json:"line,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Discovery struct {
	Candidates []Candidate `json:"candidates"`
	Warnings   []Warning   `json:"warnings"`
	Revision   string      `json:"revision"`
}

func (d Discovery) String() string { raw, _ := json.Marshal(d); return string(raw) }

var aliasShape = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,252}$`)
var proxyJumpShape = regexp.MustCompile(`^[a-zA-Z0-9._:@,\[\]%-]+$`)

type directive struct {
	key, source string
	values      []string
	line        int
}

type configReader struct {
	ctx          context.Context
	root, home   string
	active       map[string]bool
	warnings     []Warning
	directives   []directive
	fingerprint  hash.Hash
	files, bytes int
	conditional  bool
}

// Discover reads only regular config files. Include paths use the user SSH
// directory as their base, including when an Include appears in a nested file.
// Match conditions are never evaluated, and discovery starts no subprocesses.
func Discover(ctx context.Context, path string) (Discovery, error) {
	home, err := os.UserHomeDir()
	if err != nil && path == "" {
		return Discovery{}, fmt.Errorf("定位 SSH 配置失败")
	}
	if path == "" {
		path = filepath.Join(home, ".ssh", "config")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Discovery{}, fmt.Errorf("定位 SSH 配置失败")
	}
	r := configReader{ctx: ctx, root: filepath.Dir(path), home: home, active: map[string]bool{}, warnings: []Warning{}, fingerprint: sha256.New()}
	if err := r.read(path, 0); err != nil {
		return Discovery{}, err
	}
	aliases := map[string]Candidate{}
	for _, d := range r.directives {
		if d.key != "host" {
			continue
		}
		for _, alias := range d.values {
			if !aliasShape.MatchString(alias) {
				continue
			}
			key := strings.ToLower(alias)
			if _, found := aliases[key]; !found {
				aliases[key] = Candidate{Alias: alias, HostName: alias, Port: 22, Source: d.source, Line: d.line, Conditional: r.conditional}
			}
		}
	}
	out := Discovery{Candidates: []Candidate{}, Warnings: r.warnings, Revision: hex.EncodeToString(r.fingerprint.Sum(nil))}
	for _, c := range aliases {
		active := true
		seen := map[string]bool{}
		for _, d := range r.directives {
			switch d.key {
			case "host":
				active = matchesHost(c.Alias, d.values)
			case "match":
				active = false
			default:
				if !active || seen[d.key] || len(d.values) == 0 {
					continue
				}
				seen[d.key] = true
				if d.key == "proxyjump" || d.key == "proxycommand" {
					if seen["proxy"] {
						continue
					}
					seen["proxy"] = true
				}
				value := d.values[0]
				switch d.key {
				case "hostname":
					c.HostName = value
				case "user":
					c.User = value
				case "port":
					if port, err := strconv.Atoi(value); err == nil && port > 0 && port <= 65535 {
						c.Port = port
					}
				case "proxyjump":
					if value != "none" {
						if proxyJumpShape.MatchString(value) {
							c.ProxyJump = value
						} else {
							c.ProxyJump = "已配置"
						}
					}
				case "proxycommand":
					c.HasProxyCommand = value == "configured"
				case "identityfile":
					c.HasIdentityFile = value == "configured"
				}
			}
		}
		out.Candidates = append(out.Candidates, c)
	}
	sort.Slice(out.Candidates, func(i, j int) bool { return out.Candidates[i].Alias < out.Candidates[j].Alias })
	return out, nil
}

func (r *configReader) warn(source string, line int, code, message string) {
	if len(r.warnings) >= 64 {
		return
	}
	r.warnings = append(r.warnings, Warning{Source: source, Line: line, Code: code, Message: message})
}

func (r *configReader) read(path string, depth int) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if depth > 32 || r.files >= 256 || r.bytes >= 4<<20 {
		r.warn(path, 0, "include_limit", "配置嵌套或文件总量超过读取限制，部分候选未列出")
		return nil
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		if depth == 0 && os.IsNotExist(err) {
			return nil
		}
		r.warn(path, 0, "unreadable_include", "无法读取此 SSH 配置文件")
		return nil
	}
	if r.active[canonical] {
		r.warn(path, 0, "include_cycle", "已跳过循环引用的 SSH 配置")
		return nil
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		r.warn(path, 0, "not_regular_file", "已跳过非普通配置文件")
		return nil
	}
	if info.Size() > 1<<20 || info.Size()+int64(r.bytes) > 4<<20 {
		r.warn(path, 0, "include_limit", "配置文件超过读取限制")
		return nil
	}
	f, err := os.Open(canonical)
	if err != nil {
		r.warn(path, 0, "unreadable_include", "无法读取此 SSH 配置文件")
		return nil
	}
	defer f.Close()
	// LimitReader also bounds a regular file that grows while being read.
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		r.warn(path, 0, "unreadable_include", "无法完整读取此 SSH 配置文件")
		return nil
	}
	r.active[canonical] = true
	defer delete(r.active, canonical)
	r.files++
	r.bytes += len(raw)
	fmt.Fprintf(r.fingerprint, "%d:%s:%d:", len(canonical), canonical, len(raw))
	_, _ = r.fingerprint.Write(raw)
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		if err := r.ctx.Err(); err != nil {
			return err
		}
		key, tail := splitDirective(scanner.Text())
		if key == "" {
			continue
		}
		d := directive{key: key, source: path, line: line}
		switch key {
		case "match":
			r.conditional = true
			r.directives = append(r.directives, d)
			continue
		case "proxycommand", "identityfile":
			value := "configured"
			if strings.EqualFold(strings.TrimSpace(tail), "none") {
				value = "none"
			}
			d.values = []string{value}
			r.directives = append(r.directives, d)
			continue
		case "host", "include", "hostname", "user", "port", "proxyjump":
		default:
			continue
		}
		values, ok := configWords(tail)
		if !ok {
			r.warn(path, line, "invalid_directive", "配置行的引号或转义不完整，已跳过")
			continue
		}
		d.values = values
		if key != "include" {
			r.directives = append(r.directives, d)
			continue
		}
		for _, pattern := range values {
			if strings.HasPrefix(pattern, "~/") {
				pattern = filepath.Join(r.home, pattern[2:])
			} else if strings.ContainsAny(pattern, "~%$") {
				r.warn(path, line, "dynamic_include", "包含动态路径的 Include 需由 SSH 在连接时解析")
				r.conditional = true
				continue
			} else if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(r.root, pattern)
			}
			matches, err := filepath.Glob(pattern)
			if err != nil {
				r.warn(path, line, "invalid_include", "Include 路径模式无效")
				continue
			}
			for _, include := range matches {
				if err := r.read(include, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if scanner.Err() != nil {
		r.warn(path, line, "unreadable_include", "无法完整读取此 SSH 配置文件")
	}
	return nil
}

func splitDirective(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", ""
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), ""
	}
	key := strings.ToLower(line[:i])
	rest := strings.TrimSpace(line[i:])
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	return key, rest
}

func configWords(s string) ([]string, bool) {
	words := []string{}
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, ch := range s {
		if escaped {
			word.WriteRune(ch)
			escaped = false
			started = true
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote, started = ch, true
		case '#':
			if started {
				words = append(words, word.String())
			}
			return words, true
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}

func matchesHost(alias string, patterns []string) bool {
	matched := false
	for _, pattern := range patterns {
		negative := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		ok, _ := filepath.Match(strings.ToLower(pattern), strings.ToLower(alias))
		if ok && negative {
			return false
		}
		matched = matched || ok
	}
	return matched
}
