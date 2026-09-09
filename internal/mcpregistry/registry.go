// Package mcpregistry reads the official MCP Registry and turns one of
// its entries into a server setting a machine can run: which command to
// start (by the package's runtime) or which URL to reach, and which
// environment variables the owner has to supply.
package mcpregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// BaseURL is the registry; a test points it elsewhere.
var BaseURL = "https://registry.modelcontextprotocol.io"

// Entry is one server as the registry lists it, reduced to what the page
// needs to show and Plan needs to read.
type Entry struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version,omitempty"`
	Repository  string    `json:"repository,omitempty"`
	Packages    []Package `json:"packages"`
	Remotes     []Remote  `json:"remotes"`
}

// Package is one way to run the server locally.
type Package struct {
	RegistryType string   `json:"registry_type"` // npm | pypi | oci | nuget | mcpb
	Identifier   string   `json:"identifier"`
	Version      string   `json:"version,omitempty"`
	RuntimeHint  string   `json:"runtime_hint,omitempty"` // npx | uvx | docker | dnx
	Transport    string   `json:"transport,omitempty"`    // stdio | streamable-http | sse
	RuntimeArgs  []string `json:"runtime_args,omitempty"`
	PackageArgs  []string `json:"package_args,omitempty"`
	Env          []EnvVar `json:"env"`
	// Needs is the command the machine must have to run this package.
	Needs string `json:"needs,omitempty"`
}

// Remote is one hosted endpoint.
type Remote struct {
	Type    string   `json:"type"` // streamable-http | sse
	URL     string   `json:"url"`
	Headers []EnvVar `json:"headers"`
}

// EnvVar is one variable or header the server takes.
type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Default     string `json:"default,omitempty"`
}

// Search asks the registry for servers whose name contains q.
func Search(ctx context.Context, q string, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	u := BaseURL + "/v0/servers?limit=" + fmt.Sprint(limit)
	if q = strings.TrimSpace(q); q != "" {
		u += "&search=" + url.QueryEscape(q)
	}
	// The registry answers in seconds from some networks and in tens of
	// seconds from others; a page can wait that long once.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("registry: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return Parse(io.LimitReader(resp.Body, 8<<20))
}

// Parse reads a registry listing.
func Parse(r io.Reader) ([]Entry, error) {
	var doc struct {
		Servers []struct {
			Server rawServer `json:"server"`
		} `json:"servers"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("registry: bad listing: %w", err)
	}
	out := make([]Entry, 0, len(doc.Servers))
	for _, s := range doc.Servers {
		out = append(out, s.Server.entry())
	}
	return out, nil
}

type rawArg struct {
	Type    string `json:"type"` // positional | named
	Name    string `json:"name"`
	Value   string `json:"value"`
	Default string `json:"default"`
}

type rawEnv struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsRequired  bool   `json:"isRequired"`
	IsSecret    bool   `json:"isSecret"`
	Default     string `json:"default"`
}

type rawServer struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Repository  struct {
		URL string `json:"url"`
	} `json:"repository"`
	Packages []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Version      string `json:"version"`
		RuntimeHint  string `json:"runtimeHint"`
		Transport    struct {
			Type string `json:"type"`
		} `json:"transport"`
		RuntimeArgs []rawArg `json:"runtimeArguments"`
		PackageArgs []rawArg `json:"packageArguments"`
		Env         []rawEnv `json:"environmentVariables"`
	} `json:"packages"`
	Remotes []struct {
		Type    string   `json:"type"`
		URL     string   `json:"url"`
		Headers []rawEnv `json:"headers"`
	} `json:"remotes"`
}

func (s rawServer) entry() Entry {
	e := Entry{Name: s.Name, Description: s.Description, Version: s.Version, Repository: s.Repository.URL, Packages: []Package{}, Remotes: []Remote{}}
	for _, p := range s.Packages {
		pkg := Package{RegistryType: p.RegistryType, Identifier: p.Identifier, Version: p.Version, RuntimeHint: p.RuntimeHint, Transport: p.Transport.Type, RuntimeArgs: args(p.RuntimeArgs), PackageArgs: args(p.PackageArgs), Env: envs(p.Env)}
		pkg.Needs = needs(pkg)
		e.Packages = append(e.Packages, pkg)
	}
	for _, r := range s.Remotes {
		e.Remotes = append(e.Remotes, Remote{Type: r.Type, URL: r.URL, Headers: envs(r.Headers)})
	}
	return e
}

func args(in []rawArg) []string {
	var out []string
	for _, a := range in {
		v := a.Value
		if v == "" {
			v = a.Default
		}
		if a.Type == "named" && a.Name != "" {
			out = append(out, a.Name)
			if v != "" {
				out = append(out, v)
			}
			continue
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envs(in []rawEnv) []EnvVar {
	out := make([]EnvVar, 0, len(in))
	for _, v := range in {
		out = append(out, EnvVar{Name: v.Name, Description: v.Description, Required: v.IsRequired, Secret: v.IsSecret, Default: v.Default})
	}
	return out
}

// needs is the command a machine must have for a package: the registry's
// runtime hint, else what the package type implies.
func needs(p Package) string {
	if p.RuntimeHint != "" {
		return p.RuntimeHint
	}
	switch p.RegistryType {
	case "npm":
		return "npx"
	case "pypi":
		return "uvx"
	case "oci":
		return "docker"
	case "nuget":
		return "dnx"
	}
	return ""
}

// Setting is what a machine runs: the same shape as its own MCP settings.
type Setting struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Plan turns one package of an entry into a setting. It fills the
// command from the runtime and the arguments the registry lists; the
// environment is the caller's to add from what the owner typed.
func Plan(p Package) (Setting, error) {
	if p.Transport != "" && p.Transport != "stdio" {
		return Setting{}, fmt.Errorf("package %s speaks %s, not stdio; use its remote instead", p.Identifier, p.Transport)
	}
	s := Setting{Type: "stdio"}
	switch needs(p) {
	case "npx":
		s.Command = "npx"
		s.Args = append(s.Args, p.RuntimeArgs...)
		if !slices.Contains(s.Args, "-y") && !slices.Contains(s.Args, "--yes") {
			s.Args = append(s.Args, "-y")
		}
		s.Args = append(s.Args, identifierAt(p.Identifier, p.Version))
	case "uvx":
		s.Command = "uvx"
		s.Args = append(s.Args, p.RuntimeArgs...)
		s.Args = append(s.Args, identifierAt(p.Identifier, p.Version))
	case "docker":
		s.Command = "docker"
		s.Args = append(s.Args, "run", "-i", "--rm")
		s.Args = append(s.Args, p.RuntimeArgs...)
		for _, v := range p.Env {
			s.Args = append(s.Args, "-e", v.Name)
		}
		s.Args = append(s.Args, p.Identifier)
	case "dnx":
		s.Command = "dnx"
		s.Args = append(s.Args, p.RuntimeArgs...)
		s.Args = append(s.Args, identifierAt(p.Identifier, p.Version), "--yes")
	default:
		return Setting{}, fmt.Errorf("package type %q has no known runtime", p.RegistryType)
	}
	s.Args = append(s.Args, p.PackageArgs...)
	return s, nil
}

// PlanRemote turns a hosted endpoint into a setting; headers are the
// caller's to add from what the owner typed.
func PlanRemote(r Remote) (Setting, error) {
	typ := "http"
	if r.Type == "sse" {
		typ = "sse"
	}
	if !strings.HasPrefix(r.URL, "https://") && !strings.HasPrefix(r.URL, "http://") {
		return Setting{}, fmt.Errorf("remote %q is not an http(s) URL", r.URL)
	}
	return Setting{Type: typ, URL: r.URL}, nil
}

func identifierAt(id, version string) string {
	if version == "" {
		return id
	}
	return id + "@" + version
}
