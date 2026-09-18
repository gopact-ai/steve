package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

// A desktop launcher that replaced the installed application asks the hub to
// come back on the program it now finds there. Rebuilding the application in
// place would answer from the build already loaded and report an upgrade that
// never happened, so the peer has to stop and let the process continue as the
// named program.
func TestUpgradeRestartStopsThePeerToRunTheInstalledProgram(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	program := filepath.Join(t.TempDir(), "steve")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/services/hub/restart", consoleapi.RestartRequest{CommandID: "desktop-upgrade", Program: program})
	if status != http.StatusOK {
		t.Fatalf("upgrade request: %d %s", status, body)
	}
	var receipt consoleapi.RestartOperation
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.State != "accepted" {
		t.Fatalf("upgrade receipt: %+v %v", receipt, err)
	}
	select {
	case err := <-peer.Errors:
		var restart *adminsvc.RestartExit
		if !errors.As(err, &restart) {
			t.Fatalf("the peer stopped for another reason: %v", err)
		}
		if restart.Program != program {
			t.Fatalf("the process was left to run %q instead of the installed program", restart.Program)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the peer kept running its own build instead of handing the process over")
	}
	if !peer.Runtime.Load().Status().Closed {
		t.Fatal("the runtime rebuilt itself in place, so the new program would never be loaded")
	}
}

// A program the service could not become is refused while the launcher is
// still listening, rather than after the service has already stopped.
func TestUpgradeRestartRefusesAProgramItCannotRun(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	missing := filepath.Join(t.TempDir(), "steve")
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/services/hub/restart", consoleapi.RestartRequest{CommandID: "desktop-upgrade-missing", Program: missing})
	if status == http.StatusOK {
		t.Fatalf("a missing program was accepted: %s", body)
	}
	if peer.Runtime.Load().Status().Closed {
		t.Fatal("a refused upgrade stopped the service")
	}
}
