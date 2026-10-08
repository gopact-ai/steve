//go:build linux || darwin

package acphost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/procgroup"
)

const TerminalChildVerb = "--acp-terminal-child"

// terminalChildConfig is private parent-to-child data, not a shared record.
// No payload is executed until both explicit gates arrive on the inherited FD.
type terminalChildConfig struct {
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Env         []string `json:"env"`
	ParentGroup int      `json:"parent_group"`
	ParentMark  string   `json:"parent_mark"`
	Mark        string   `json:"mark"`
}

type terminalChildNotice struct {
	Phase string `json:"phase"`
	PID   int    `json:"pid"`
	Group int    `json:"group"`
}

// RunTerminalChild handles only a trusted inherited-descriptor launch. Calling
// the ordinary binary with this verb alone does not supply execution authority.
// The helper performs no provider/config lookup or payload before its gates.
func RunTerminalChild(args []string) (bool, error) {
	if len(args) == 0 || args[0] != TerminalChildVerb {
		return false, nil
	}
	if len(args) != 1 {
		return true, errors.New("terminal child accepts no command-line payload")
	}
	return true, runTerminalChild()
}

func runTerminalChild() (result error) {
	// ExtraFiles are inherited in blocking mode. Register a pollable control
	// pipe before wrapping it so the bounded inert deadline is real.
	if err := syscall.SetNonblock(3, true); err != nil {
		return err
	}
	control := os.NewFile(3, "terminal-control")
	cwd := os.NewFile(4, "terminal-cwd")
	status := os.NewFile(5, "terminal-status")
	if control == nil || cwd == nil || status == nil {
		return errors.New("terminal child requires inherited owner descriptors")
	}
	defer control.Close()
	defer cwd.Close()
	defer status.Close()
	defer func() {
		if result != nil {
			_ = json.NewEncoder(status).Encode(terminalChildNotice{Phase: "failed", PID: os.Getpid(), Group: syscall.Getpgrp()})
		}
	}()
	// EOF is an exec acknowledgement only after the descriptor becomes
	// close-on-exec; launch failures leave a bounded explicit notice.
	syscall.CloseOnExec(5)
	syscall.CloseOnExec(6)
	info, err := cwd.Stat()
	if err != nil || !info.IsDir() {
		return errors.New("terminal child requires its pinned workspace")
	}
	if info, err := control.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("terminal child control is not its owner pipe")
	}
	if info, err := status.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("terminal child status is not its owner pipe")
	}
	// An inert helper abandoned before admission must not persist forever.
	if err := control.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(control, 16<<10)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 16<<10 {
		return errors.New("terminal child configuration is unavailable")
	}
	var config terminalChildConfig
	if err := json.Unmarshal(line, &config); err != nil {
		return errors.New("terminal child configuration is invalid")
	}
	if config.Command == "" || strings.ContainsRune(config.Command, 0) ||
		len(config.Command) > 4096 || len(config.Args) > 128 || len(config.Env) > 128 ||
		config.ParentGroup <= 0 || config.ParentMark == "" || config.Mark == "" ||
		len(config.ParentMark) > 128 || len(config.Mark) > 128 ||
		syscall.Getpgrp() != config.ParentGroup ||
		os.Getenv(procgroup.MarkVariable) != config.ParentMark {
		return errors.New("terminal child differs from its original group")
	}
	for _, arg := range config.Args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("terminal child argument is invalid")
		}
	}
	for _, item := range config.Env {
		if !strings.ContainsRune(item, '=') || strings.ContainsRune(item, 0) {
			return errors.New("terminal child environment is invalid")
		}
	}
	if err := json.NewEncoder(status).Encode(terminalChildNotice{
		Phase: "preparing", PID: os.Getpid(), Group: syscall.Getpgrp(),
	}); err != nil {
		return err
	}
	gate, err := reader.ReadSlice('\n')
	if err != nil || string(gate) != "SPLIT\n" {
		return errors.New("terminal child group transition was not admitted")
	}
	// No fork/exec is possible between this split and its durable active gate.
	if err := syscall.Setpgid(0, 0); err != nil {
		return err
	}
	if err := os.Setenv(procgroup.MarkVariable, config.Mark); err != nil {
		return err
	}
	if err := json.NewEncoder(status).Encode(terminalChildNotice{
		Phase: "active", PID: os.Getpid(), Group: syscall.Getpgrp(),
	}); err != nil {
		return err
	}
	gate, err = reader.ReadSlice('\n')
	if err != nil || string(gate) != "EXEC\n" {
		return errors.New("terminal child payload was not admitted")
	}
	if err := syscall.Fchdir(int(cwd.Fd())); err != nil {
		return err
	}
	if err := control.Close(); err != nil {
		return err
	}
	if err := cwd.Close(); err != nil {
		return err
	}
	path := config.Command
	if !strings.ContainsRune(path, '/') {
		// The payload uses the original frozen profile PATH, never the
		// helper's unrelated lookup environment.
		value := ""
		for _, item := range config.Env {
			if strings.HasPrefix(item, "PATH=") {
				value = strings.TrimPrefix(item, "PATH=")
			}
		}
		if err := os.Setenv("PATH", value); err != nil {
			return err
		}
		var err error
		path, err = exec.LookPath(path)
		if err != nil {
			return errors.New("terminal command is unavailable in its profile")
		}
	}
	env := withoutEnv(config.Env, procgroup.MarkVariable)
	env = append(env, procgroup.MarkVariable+"="+config.Mark)
	argv := append([]string{config.Command}, config.Args...)
	if err := json.NewEncoder(status).Encode(terminalChildNotice{Phase: "executing", PID: os.Getpid(), Group: syscall.Getpgrp()}); err != nil {
		return err
	}
	if err := syscall.Exec(path, argv, env); err != nil {
		// Do not reflect environment, command payload or credential-bearing
		// argument values in a helper failure.
		return errors.New("terminal command could not be executed")
	}
	return nil
}

func writeTerminalChildGate(writer io.Writer, config terminalChildConfig) error {
	raw, err := json.Marshal(config)
	if err != nil || len(raw) > 16<<10-1 {
		return errors.New("terminal child configuration exceeds its bound")
	}
	_, err = writer.Write(append(raw, '\n'))
	return err
}

func terminalNotice(reader *bufio.Reader, phase string, pid int) error {
	raw, err := reader.ReadSlice('\n')
	if err != nil || len(raw) > 4096 {
		return errors.New("terminal child admission notice is unavailable")
	}
	var notice terminalChildNotice
	if err := json.Unmarshal(raw, &notice); err != nil || notice.Phase != phase ||
		notice.PID != pid || notice.Group <= 0 {
		return errors.New("terminal child admission notice differs")
	}
	if (phase == "active" || phase == "executing") && notice.Group != pid {
		return errors.New("terminal child does not lead its admitted group")
	}
	return nil
}

func terminalChildError(err error) {
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "terminal child failed:", strconv.Quote(err.Error()))
	}
}
