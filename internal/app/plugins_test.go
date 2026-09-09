package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPluginCommandsPreviewPrepareReplayListAndVerify(t *testing.T) {
	ctx := t.Context()
	source := filepath.Join("..", "..", "examples", "plugins", "github")
	invoke := func(args ...string) []byte {
		t.Helper()
		var out, diagnostic bytes.Buffer
		if err := runPluginsCommand(ctx, args, &out, &diagnostic); err != nil {
			t.Fatalf("command %v: %v (%s)", args, err, diagnostic.String())
		}
		return out.Bytes()
	}
	preview := invoke("preview", "-source", source)
	var bundle plugins.Bundle
	if err := json.Unmarshal(preview, &bundle); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "store")
	args := []string{"prepare", "-source", source, "-store", store, "-command-id", "review-install", "-digest", bundle.Digest}
	first := invoke(args...)
	second := invoke(args...)
	if !bytes.Equal(first, second) {
		t.Fatalf("receipt changed: %s / %s", first, second)
	}
	var receipt plugins.Receipt
	if err := json.Unmarshal(first, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != plugins.Prepared || receipt.Digest != bundle.Digest {
		t.Fatalf("not prepared: %+v", receipt)
	}
	var receipts []plugins.Receipt
	if err := json.Unmarshal(invoke("list", "-store", store), &receipts); err != nil || len(receipts) != 1 {
		t.Fatalf("list: %v %+v", err, receipts)
	}
	var shown plugins.Bundle
	if err := json.Unmarshal(invoke("show", "-store", store, "-digest", bundle.Digest), &shown); err != nil || shown.Digest != bundle.Digest {
		t.Fatalf("show: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(store))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "store" {
		t.Fatalf("command touched unrelated files: %v", entries)
	}
}

func TestPluginCommandRefusalsDoNotPrepareContent(t *testing.T) {
	for _, args := range [][]string{
		nil, {"activate"}, {"preview"}, {"prepare", "-source", "missing"}, {"list"}, {"show", "-store", t.TempDir()}, {"list", "-store", t.TempDir(), "extra"}, {"preview", "-source", "missing", "-commit", "main"},
	} {
		var out, diagnostic bytes.Buffer
		if err := runPluginsCommand(t.Context(), args, &out, &diagnostic); err == nil || out.Len() != 0 {
			t.Fatalf("%v: %v output %s", args, err, out.String())
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := runPluginsCommand(ctx, []string{"preview", "-source", filepath.Join("..", "..", "examples", "plugins", "github")}, &out, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestPluginCommandsNameMissingRequiredFlagsBeforeAccessingFiles(t *testing.T) {
	for _, missing := range []struct {
		action, flag string
		flags        []string
	}{
		{"prepare", "-command-id", []string{"-digest", strings.Repeat("a", 64)}},
		{"prepare", "-digest", []string{"-command-id", "prepare"}},
		{"show", "-digest", nil},
	} {
		t.Run(missing.action+missing.flag, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "store")
			args := []string{missing.action, "-store", store}
			if missing.action == "prepare" {
				args = append(args, "-source", filepath.Join(t.TempDir(), "absent-source"))
			}
			args = append(args, missing.flags...)
			var out, diagnostic bytes.Buffer
			err := runPluginsCommand(t.Context(), args, &out, &diagnostic)
			want := "plugins " + missing.action + " requires " + missing.flag
			if err == nil || err.Error() != want || out.Len() != 0 {
				t.Fatalf("missing flag: %v output=%q, want %q", err, out.String(), want)
			}
			if _, err := os.Lstat(store); !os.IsNotExist(err) {
				t.Fatalf("invalid command touched the store: %v", err)
			}
		})
	}
}
