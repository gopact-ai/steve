package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/plugins"
)

func TestLocalPluginSecretCLIPrintsOnlyReference(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins")
	var out, diagnostic bytes.Buffer
	err := runPluginSecretCommand(t.Context(), []string{"secret-put", "-store", dir, "-name", "team"}, strings.NewReader("private-cli-value\n"), &out, &diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String()+diagnostic.String(), "private-cli-value") {
		t.Fatal("CLI echoed secret")
	}
	var info plugins.SecretInfo
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	value, err := (&plugins.Store{Dir: dir}).Secret(info.Reference)
	if err != nil || value != "private-cli-value" {
		t.Fatal("node secret was not stored")
	}
	out.Reset()
	if err := runPluginSecretCommand(t.Context(), []string{"secret-list", "-store", dir}, nil, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-cli-value") {
		t.Fatal("list leaked secret")
	}
}
