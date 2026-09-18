package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// upgradeSettle bounds how long the launcher holds the window back while a
// service that had nothing to finish restarts onto the new program. Past
// it the window opens on the build that is still answering; the restart
// stays scheduled and applies itself when the service falls idle.
const upgradeSettle = 12 * time.Second

// applyReplacedProgram brings a service started from an earlier build onto
// the one this launcher was started from. The restart is requested as a
// waiting one, so a conversation in progress is finished rather than cut
// short, and the launcher only holds the window back while a service that
// was already idle comes back.
//
// Failing to schedule the upgrade is reported with the running service
// rather than raised: an application that cannot upgrade still opens.
func applyReplacedProgram(ctx context.Context, installed *Installation, program string, result LaunchResult) LaunchResult {
	target := nodewire.Version()
	if result.Version == "" || target == "" || result.Version == target {
		return result
	}
	status := &UpgradeStatus{From: result.Version, To: target}
	result.Upgrade = status
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	id, err := upgradeCommandID(installed, target)
	if err != nil {
		status.Error = err.Error()
		return result
	}
	op, err := requestUpgrade(ctx, client, installed, result.URL, id, program)
	if err != nil {
		status.Error = err.Error()
		return result
	}
	status.WaitingOn = op.WaitingOn
	return awaitUpgrade(ctx, client, installed, result, id)
}

// upgradeCommandID is stable for one running service and one target build,
// so relaunching the application joins the request it already made instead
// of queueing another.
func upgradeCommandID(installed *Installation, target string) (string, error) {
	var current endpoint
	if err := readJSON(installed.Paths.Endpoint, &current); err != nil {
		return "", err
	}
	if current.Instance == "" {
		return "", errors.New("the running service has no instance identity")
	}
	sum := sha256.Sum256([]byte(current.Instance + "\x00" + target))
	return "desktop-upgrade-" + hex.EncodeToString(sum[:10]), nil
}

// requestUpgrade names the program this launcher was started from, so the
// service continues as the build that is installed now rather than
// rebuilding itself on the one it already runs.
func requestUpgrade(ctx context.Context, client *http.Client, installed *Installation, address, id, program string) (consoleapi.RestartOperation, error) {
	body, err := json.Marshal(consoleapi.RestartRequest{CommandID: id, Mode: consoleapi.RestartWhenIdle, Program: program})
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	return callUpgrade(ctx, client, installed, http.MethodPost, address+"/console/services/hub/restart", body)
}

func upgradeStatus(ctx context.Context, client *http.Client, installed *Installation, address, id string) (consoleapi.RestartOperation, error) {
	return callUpgrade(ctx, client, installed, http.MethodGet, address+"/console/services/hub/restart?command_id="+id, nil)
}

func callUpgrade(ctx context.Context, client *http.Client, installed *Installation, method, address string, body []byte) (consoleapi.RestartOperation, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+installed.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return consoleapi.RestartOperation{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct{ Error string }
		if json.Unmarshal(raw, &failure) == nil && failure.Error != "" {
			return consoleapi.RestartOperation{}, fmt.Errorf("schedule upgrade: %s", failure.Error)
		}
		return consoleapi.RestartOperation{}, fmt.Errorf("schedule upgrade: HTTP %d", resp.StatusCode)
	}
	var op consoleapi.RestartOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return consoleapi.RestartOperation{}, err
	}
	return op, nil
}

// awaitUpgrade waits only for a restart that is applying now: a service
// with work to finish reports what it is waiting on and the launcher stops
// holding the window back.
func awaitUpgrade(ctx context.Context, client *http.Client, installed *Installation, result LaunchResult, id string) LaunchResult {
	deadline := time.Now().Add(upgradeSettle)
	if due, ok := ctx.Deadline(); ok && due.Add(-3*time.Second).Before(deadline) {
		deadline = due.Add(-3 * time.Second)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return result
		case <-time.After(250 * time.Millisecond):
		}
		var current endpoint
		if readJSON(installed.Paths.Endpoint, &current) == nil && current.NodeID == installed.NodeID && processAlive(current.PID) {
			if version, err := probeBackend(ctx, client, installed, current); err == nil {
				if version == result.Upgrade.To {
					result.URL, result.PID, result.Version = current.URL, current.PID, version
					result.Upgrade.Applied, result.Upgrade.WaitingOn = true, ""
					return result
				}
				op, err := upgradeStatus(ctx, client, installed, current.URL, id)
				if err == nil && op.State == nodewire.RestartStateDraining && op.WaitingOn != "" && op.WaitingOn != consoleapi.RestartWaitPreparing {
					result.Upgrade.WaitingOn = op.WaitingOn
					return result
				}
				if err == nil && op.State == nodewire.RestartStateFailed {
					result.Upgrade.Error, result.Upgrade.WaitingOn = op.Error, ""
					return result
				}
			}
		}
	}
	return result
}
