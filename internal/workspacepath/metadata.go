// Package workspacepath compares workspace metadata, not local filesystem paths.
// Fully-qualified drives and backslash-prefixed UNC shares have Windows lexical
// syntax on any hub OS. POSIX paths keep literal backslashes; // is still POSIX.
// Only qualified drive letters fold to uppercase; component and UNC case stays.
// These comparisons neither resolve files nor authorize access on any node.
package workspacepath

import (
	"path"
	"strings"
)

type metadata struct {
	mode   byte
	volume string
	name   string
}

func windowsSeparator(r rune) bool { return r == '/' || r == '\\' }

func windowsPath(volume, tail string) metadata {
	parts := strings.FieldsFunc(tail, windowsSeparator)
	return metadata{mode: 'w', volume: volume, name: path.Clean("/" + strings.Join(parts, "/"))}
}

func parse(value string) metadata {
	if len(value) >= 2 && value[1] == ':' && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') {
		if len(value) >= 3 && windowsSeparator(rune(value[2])) {
			drive := value[0]
			if drive >= 'a' && drive <= 'z' {
				drive -= 'a' - 'A'
			}
			return windowsPath(string([]byte{drive, ':'}), value[3:])
		}
		// Drive-relative paths never gain an implied current drive directory.
		return metadata{mode: 'o', name: value}
	}
	if strings.HasPrefix(value, `\\`) {
		server, rest, ok := cutWindowsComponent(value[2:])
		share, tail, _ := cutWindowsComponent(rest)
		if ok && server != "" && server != "." && server != ".." && server != "?" && share != "" && share != "." && share != ".." {
			return windowsPath(`\\`+server+`\`+share, tail)
		}
		// Incomplete shares and device namespaces stay opaque, not local paths.
		return metadata{mode: 'o', name: value}
	}
	if strings.HasPrefix(value, `\`) {
		return metadata{mode: 'o', name: value}
	}
	return metadata{mode: 'p', name: path.Clean(value)}
}

func cutWindowsComponent(value string) (head, tail string, found bool) {
	index := strings.IndexFunc(value, windowsSeparator)
	if index < 0 {
		return value, "", false
	}
	return value[:index], value[index+1:], true
}

// IsAbs requires an explicit drive root, UNC server/share, or POSIX root.
// It does not infer a remote OS from the hub's filepath implementation.
func IsAbs(value string) bool {
	p := parse(value)
	return p.mode == 'w' || p.mode == 'p' && path.IsAbs(p.name)
}

// Base returns the last lexical component within the declared path syntax.
func Base(value string) string {
	p := parse(value)
	if p.mode != 'w' {
		return path.Base(value)
	}
	tail := strings.TrimRight(value[len(p.volume):], `/\`)
	if tail == "" {
		return "/"
	}
	return tail[strings.LastIndexAny(tail, `/\`)+1:]
}

// Dir returns a lexical parent without crossing a drive or UNC share root.
func Dir(value string) string {
	p := parse(value)
	if p.mode == 'w' {
		tail := strings.ReplaceAll(value[len(p.volume):], `\`, "/")
		parent := path.Dir("/" + tail)
		return p.volume + strings.ReplaceAll(parent, "/", `\`)
	}
	return path.Dir(value)
}

// Same excludes empty locations. It preserves component and UNC case and does
// not resolve symlinks. Unsupported Windows spellings match only exact text.
func Same(a, b string) bool {
	return a != "" && b != "" && parse(a) == parse(b)
}

// Overlap compares lexical component boundaries only within the same syntax
// and volume. Windows separator conversion is confined to qualified paths.
func Overlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left, right := parse(a), parse(b)
	if left.mode != right.mode || left.volume != right.volume {
		return false
	}
	if left.name == right.name {
		return true
	}
	return left.mode != 'o' && (strings.HasPrefix(left.name, strings.TrimSuffix(right.name, "/")+"/") ||
		strings.HasPrefix(right.name, strings.TrimSuffix(left.name, "/")+"/"))
}
