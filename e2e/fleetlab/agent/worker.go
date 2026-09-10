package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type job struct{ Kind, Path, Content, Release, Barrier, Token string }

var jobLine = regexp.MustCompile(`FLEETLAB_JOB=([A-Za-z0-9_-]+)`)
var safeRelease = regexp.MustCompile(`^release-[A-Za-z0-9-]+$`)

func parseJob(input string) (job, bool, error) {
	m := jobLine.FindStringSubmatch(input)
	if len(m) == 0 {
		return job{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[1])
	if err != nil {
		return job{}, true, err
	}
	var j job
	err = json.Unmarshal(raw, &j)
	return j, true, err
}
func work(ctx context.Context, cwd string, j job) (string, error) {
	if j.Kind == "file" {
		if !filepath.IsLocal(j.Path) || filepath.Base(j.Path) != j.Path {
			return "", errors.New("job requires a root-relative filename")
		}
		path := filepath.Join(cwd, j.Path)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return "", err
		}
		_, err = f.WriteString(j.Content)
		err = errors.Join(err, f.Close())
		return "Created " + j.Path, err
	}
	if (j.Kind != "build" && j.Kind != "docs") || !safeRelease.MatchString(j.Release) {
		return "", errors.New("unrecognized release job")
	}
	if err := arrive(ctx, j); err != nil {
		return "", err
	}
	dir := filepath.Join(cwd, "kvtool", j.Release)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	switch j.Kind {
	case "build":
		cmd := exec.CommandContext(ctx, "/opt/go/bin/go", "build", "-o", filepath.Join(dir, "kvtool"), ".")
		cmd.Dir = filepath.Join(cwd, "kvtool")
		cmd.Env = append(os.Environ(), "GOROOT=/opt/go", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build on worker: %w: %s", err, out)
		}
		cmd = exec.CommandContext(ctx, "sha256sum", "kvtool")
		cmd.Dir = dir
		sums, err := cmd.Output()
		if err != nil {
			return "", err
		}
		if err = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums, 0644); err != nil {
			return "", err
		}
	case "docs":
		f, err := os.OpenFile(filepath.Join(cwd, "kvtool", "README.md"), os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return "", err
		}
		_, err = f.WriteString("\n## 安装与校验\n\n下载发布目录后运行 `sha256sum -c SHA256SUMS`，再安装 kvtool。\n")
		if err = errors.Join(err, f.Close()); err != nil {
			return "", err
		}
		if err = os.WriteFile(filepath.Join(dir, "RELEASE.md"), []byte("# "+j.Release+"\n\nLinux amd64 release of kvtool.\n"), 0644); err != nil {
			return "", err
		}
	}
	return "Completed " + j.Kind + " for " + j.Release, nil
}
func arrive(ctx context.Context, j job) error {
	if !strings.HasPrefix(j.Barrier, "http://") || j.Token == "" {
		return errors.New("missing lab rendezvous")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.Barrier+"/"+j.Release+"/"+j.Kind, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+j.Token)
	client := &http.Client{Timeout: 90 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("lab rendezvous: %s", resp.Status)
	}
	return nil
}
