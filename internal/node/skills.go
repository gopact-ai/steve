package node

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

// SkillsDir holds materialized bundles, one directory per hash, and
// "current" naming the one the harness homes link to.
func (s *Server) SkillsDir() string { return filepath.Join(s.conf().StateDir, "skills") }

func (s *Server) currentSkills() string {
	b, err := os.ReadFile(filepath.Join(s.SkillsDir(), "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) skillEntries() []skills.Entry {
	hash := s.currentSkills()
	if hash == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.SkillsDir(), hash, ".manifest.json"))
	if err != nil {
		return nil
	}
	var entries []skills.Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil
	}
	return entries
}

// applySkills takes the bundle the hub just put in the blob directory,
// checks it is the bundle it claims to be, unpacks it next to the other
// bundles and links every skill into every harness home. Old bundles go
// once the new one is current. New sessions see the new skills; running
// ones keep what they started with until the hub restarts them.
func (s *Server) applySkills(stream *nodewire.Stream) {
	defer stream.Close()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	verb, hash, _ := strings.Cut(stream.Request().Command, " ")
	hash = strings.TrimSpace(hash)
	fail := func(code string, err error) {
		log.Printf("steve-node: skills %s: %v", hash, err)
		fmt.Fprintln(stream, err.Error())
		closeStream(stream, nodewire.ExitPrefix+code)
	}
	if verb != "apply" || hash == "" || hash != filepath.Base(hash) {
		fail("2", fmt.Errorf("bad request %q", stream.Request().Command))
		return
	}
	blob := filepath.Join(s.BlobDir(), "skills-"+hash+".tar")
	data, err := os.ReadFile(blob)
	if err != nil {
		fail("1", fmt.Errorf("bundle not received: %w", err))
		return
	}
	if got := skills.HashOf(data); got != hash {
		fail("1", fmt.Errorf("bundle hash is %s, not %s", got[:12], hash[:12]))
		return
	}
	dir := filepath.Join(s.SkillsDir(), hash)
	staging := dir + ".staging"
	// Leftovers of an earlier attempt are cleared best-effort: whatever
	// survives makes the unpack below fail with the real reason.
	_ = os.RemoveAll(staging)
	entries, err := skills.Unpack(data, staging)
	if err != nil {
		// Cleanup after a failed unpack; the unpack error is the answer.
		_ = os.RemoveAll(staging)
		fail("1", fmt.Errorf("unpack: %w", err))
		return
	}
	// Entries are plain strings and hashes; encoding them cannot fail.
	manifest, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(staging, ".manifest.json"), manifest, 0o600); err != nil {
		fail("1", err)
		return
	}
	// A stale bundle of the same hash is replaced; if it will not go, the
	// rename reports it.
	_ = os.RemoveAll(dir)
	if err := os.Rename(staging, dir); err != nil {
		fail("1", err)
		return
	}
	if err := s.materializeSkills(hash); err != nil {
		fail("1", err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "current"), []byte(hash+"\n"), 0o600); err != nil {
		fail("1", err)
		return
	}
	// The bundle is unpacked and current; the tarball and the bundles it
	// replaces are only disk now, and the next apply clears what stays.
	_ = os.Remove(blob)
	if old, err := os.ReadDir(s.SkillsDir()); err == nil {
		for _, e := range old {
			if e.IsDir() && e.Name() != hash {
				_ = os.RemoveAll(filepath.Join(s.SkillsDir(), e.Name()))
			}
		}
	}
	log.Printf("steve-node: skills %s materialized: %d skills", hash[:12], len(entries))
	closeStream(stream, nodewire.ExitPrefix+"0")
}

// materializeSkills links every skill of the bundle into every harness
// home, replacing whatever was linked before.
func (s *Server) materializeSkills(hash string) error {
	selected := make([]string, 0, len(s.conf().Harnesses))
	for id := range s.conf().Harnesses {
		selected = append(selected, id)
	}
	return s.materializeSkillsFor(hash, selected)
}

func (s *Server) materializeSkillsFor(hash string, selected []string) error {
	dir := filepath.Join(s.SkillsDir(), hash)
	names, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, dest := range steveruntime.SelectedSkillDests(s.conf().StateDir, selected) {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		old, err := os.ReadDir(dest)
		if err != nil {
			return err
		}
		for _, e := range old {
			if err := os.RemoveAll(filepath.Join(dest, e.Name())); err != nil {
				return err
			}
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			if err := os.Symlink(filepath.Join(dir, n.Name()), filepath.Join(dest, n.Name())); err != nil {
				return fmt.Errorf("link skill %q: %w", n.Name(), err)
			}
		}
	}
	return nil
}
