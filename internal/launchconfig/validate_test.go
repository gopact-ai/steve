package launchconfig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/procgroup"
)

func TestValidationDoesNotEchoLaunchValues(t *testing.T) {
	const value = "private-fixture-value"
	for _, tc := range []struct {
		command   string
		args, env []string
	}{
		{value + "\x00", nil, nil},
		{"unknown-agent", []string{value + "\x00"}, nil},
		{"unknown-agent", nil, []string{"MODE=" + value + "\x00"}},
		{"unknown-agent", nil, []string{"BAD-KEY=" + value}},
		{"unknown-agent", nil, []string{value}},
		{"unknown-agent", nil, []string{"MODE=" + value, "MODE=other"}},
		{"unknown-agent", nil, []string{procgroup.MarkVariable + "=" + value}},
	} {
		err := Validate("", tc.command, tc.args, tc.env)
		if err == nil || strings.Contains(err.Error(), value) {
			t.Fatalf("diagnostic exposed input or accepted invalid launch: %v", err)
		}
	}
}

func TestValidationPreservesLiteralStartup(t *testing.T) {
	args := []string{"", "中文", "two words", "'$HOME'", "--unknown"}
	env := []string{"MODE=", "_CUSTOM=中文 value=tail", "Path=one", "PATH=two"}
	beforeArgs, beforeEnv := append([]string(nil), args...), append([]string(nil), env...)
	if err := Validate("", "unknown tool", args, env); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, beforeArgs) || !reflect.DeepEqual(env, beforeEnv) {
		t.Fatal("validator rewrote argv/env")
	}
}
