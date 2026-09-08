package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

// claim admits the instance's owner by name. Disconnecting or losing a hub
// never grants ownership to another; adoption changes the persisted owner
// while the instance is stopped.
func (s *Server) claim(hub string) error {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubName != "" && s.hubName != hub {
		return fmt.Errorf("this node belongs to hub %q; stop this instance and explicitly adopt %q", s.hubName, hub)
	}
	// The disk record preserves ownership across restarts. Released is
	// evidence that the old processes stopped, not permission to take over.
	if s.hubLive == 0 {
		owner, err := s.readOwner()
		if err != nil {
			return err
		}
		if owner.Hub != "" && owner.Hub != hub {
			return fmt.Errorf("this node belongs to hub %q; stop this instance and explicitly adopt %q", owner.Hub, hub)
		}
	}
	if err := s.writeOwner(hubOwner{Hub: hub, LastSeen: time.Now().UTC()}); err != nil {
		return err
	}
	s.hubName = hub
	s.hubLive++
	return nil
}

func (s *Server) release(hub string, clean bool) {
	if clean {
		clean = s.processesStopped(hub)
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubName == hub && s.hubLive > 0 {
		s.hubLive--
		if s.hubLive == 0 {
			if err := s.writeOwner(hubOwner{Hub: hub, LastSeen: time.Now().UTC(), Released: clean}); err != nil {
				log.Printf("node: persist owner release: %v", err)
			}
		}
	}
}

// hubOwner binds an instance to its hub across disconnects and restarts.
type hubOwner struct {
	Hub      string    `json:"hub"`
	LastSeen time.Time `json:"last_seen"`
	// Released records a clean disconnect with stopped processes. It can
	// supply stop evidence to offline adoption but never clears ownership.
	Released bool `json:"released,omitempty"`
}

func (s *Server) ownerPath() string { return filepath.Join(s.conf().StateDir, "hub.json") }

func (s *Server) owner() hubOwner { o, _ := s.readOwner(); return o }
func (s *Server) readOwner() (hubOwner, error) {
	var o hubOwner
	if s.conf().StateDir == "" {
		return o, nil
	}
	raw, err := os.ReadFile(s.ownerPath())
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("node ownership record unreadable: %w", err)
	}
	return o, nil
}

func (s *Server) writeOwner(o hubOwner) error {
	if s.conf().StateDir == "" {
		return nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return (&ledger.FileDocument{Path: s.ownerPath()}).Save(raw)
}

func (s *Server) processesStopped(hub string) bool {
	if s.sessions != nil && !s.sessions.processesStopped() {
		return false
	}
	s.processMu.Lock()
	defer s.processMu.Unlock()
	for _, p := range s.processes {
		if p.owner != hub {
			continue
		}
		p.mu.Lock()
		ended := p.exit != ""
		p.mu.Unlock()
		if !ended {
			return false
		}
	}
	return true
}

// touchOwner marks the serving hub as seen now; the hub's minute refresh
// keeps this current while the connection lives.
func (s *Server) touchOwner() {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubLive > 0 {
		if err := s.writeOwner(hubOwner{Hub: s.hubName, LastSeen: time.Now().UTC()}); err != nil {
			log.Printf("node: persist owner heartbeat: %v", err)
		}
	}
}

// Adopt assigns a stopped instance to the named hub. Handshakes from any
// other hub remain refused, even before the new owner first connects.
func Adopt(stateDir, hub string) error { return AdoptWithEvidence(stateDir, hub, "operator", "") }

// AdoptWithEvidence requires the instance stopped. Unfinished stream journals
// additionally require the operator's explicit physical-stop verification.
// This records that statement; it does not pretend to verify another process.
func AdoptWithEvidence(stateDir, hub, actor, evidence string) error {
	if hub == "" || stateDir == "" {
		return errors.New("adopt requires node state directory and hub identity")
	}
	unlock, err := steveruntime.AcquireLock(filepath.Join(stateDir, "instance-control"))
	if err != nil {
		return fmt.Errorf("stop the node instance before adoption: %w", err)
	}
	defer unlock()
	streams, err := os.ReadDir(filepath.Join(stateDir, "streams"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var unknown []string
	for _, stream := range streams {
		if !stream.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(stateDir, "streams", stream.Name(), "ended")); err != nil {
			unknown = append(unknown, stream.Name())
		}
	}
	if len(unknown) > 0 && strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("process streams %v have no verified exit; --evidence must record operator verification after physically stopping them", unknown)
	}
	s := &Server{}
	s.cfg.Store(&ServerConfig{StateDir: stateDir})
	previous, err := s.readOwner()
	if err != nil {
		return err
	}
	if previous.Hub != "" && previous.Hub != hub && !previous.Released && strings.TrimSpace(evidence) == "" {
		return errors.New("previous owner did not cleanly release; explicit physical stop evidence is required")
	}
	record := struct {
		From, To, Actor, Evidence string
		At                        time.Time
		UnknownStreams            []string
	}{previous.Hub, hub, actor, evidence, time.Now().UTC(), unknown}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := (&ledger.FileDocument{Path: filepath.Join(stateDir, "last-adoption.json")}).Save(raw); err != nil {
		return err
	}
	return s.writeOwner(hubOwner{Hub: hub, LastSeen: record.At, Released: true})
}
