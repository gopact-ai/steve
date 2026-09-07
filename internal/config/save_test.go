package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveReportsReplacementWhenDirectorySyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := Starter("before", "secret", "owner")
	if err := Save(path, before); err != nil {
		t.Fatal(err)
	}
	after := Starter("after", "secret", "owner")
	cause := errors.New("directory cannot sync")
	err := saveWithSync(path, after, func(string) error { return cause })
	if !Committed(err) || !errors.Is(err, cause) {
		t.Fatalf("committed error lost its outcome or cause: %v", err)
	}
	current, loadErr := Load(path)
	if loadErr != nil || current.Feishu.AppID != "after" {
		t.Fatalf("rename did not install the candidate: %v, %+v", loadErr, current)
	}
	if !strings.Contains(err.Error(), "durability uncertain") {
		t.Fatalf("uncertain durability was hidden: %v", err)
	}
}

func TestSaveFailureBeforeReplacementIsNotCommitted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := Save(path, Starter("before", "secret", "owner")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = Save(filepath.Join(path, "cannot-create.json"), Starter("after", "secret", "owner"))
	if err == nil || Committed(err) {
		t.Fatalf("pre-commit failure classified incorrectly: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(raw) {
		t.Fatal("failed save changed the existing config")
	}
	if Committed(nil) {
		t.Fatal("nil error reported as uncertain commit")
	}
}
