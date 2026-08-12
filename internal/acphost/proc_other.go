//go:build !unix

package acphost

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(*exec.Cmd) {}
