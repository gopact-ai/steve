package fleetlab_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/e2e/fleetlab"
)

func TestConfigurationErrorsAreNotUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Docker is absent for every case.
	for _, name := range []string{"NODE_A", "NODE_B"} {
		for _, field := range []string{"ADDR", "TOKEN", "HOME"} {
			t.Setenv("STEVE_LAB_"+name+"_"+field, "")
		}
	}
	specs := []fleetlab.Spec{{Name: "node-a"}, {Name: "node-b"}}
	if _, err := fleetlab.Open(specs...); !errors.Is(err, fleetlab.ErrUnavailable) {
		t.Fatalf("missing Docker = %v", err)
	}
	for _, tc := range []struct {
		name, addr, home, token, want string
		specs                         []fleetlab.Spec
	}{
		{name: "empty", want: "at least one node"},
		{name: "missing home", specs: specs, addr: "127.0.0.1:1", want: "HOME must be an absolute path"},
		{name: "relative home", specs: specs, addr: "127.0.0.1:1", home: "~", want: "HOME must be an absolute path"},
		{name: "missing token", specs: specs, addr: "127.0.0.1:1", home: "/home/lab", want: "TOKEN must not be empty"},
		{name: "partial remote", specs: specs, addr: "127.0.0.1:1", home: "/home/lab", token: "test", want: "node-b has no address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STEVE_LAB_NODE_A_ADDR", tc.addr)
			t.Setenv("STEVE_LAB_NODE_A_HOME", tc.home)
			t.Setenv("STEVE_LAB_NODE_A_TOKEN", tc.token)
			_, err := fleetlab.Open(tc.specs...)
			if err == nil || errors.Is(err, fleetlab.ErrUnavailable) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want configuration error containing %q", err, tc.want)
			}
		})
	}
}

func TestStartWithoutSpecsFailsWithoutPanic(t *testing.T) {
	if os.Getenv("STEVE_LAB_TEST_EMPTY") == "1" {
		fleetlab.Start(t)
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestStartWithoutSpecsFailsWithoutPanic$")
	cmd.Env = append(os.Environ(), "STEVE_LAB_TEST_EMPTY=1", "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "a lab needs at least one node") || strings.Contains(string(out), "panic:") {
		t.Fatalf("Start without specs = %v\n%s", err, out)
	}
}
