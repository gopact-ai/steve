package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/runtime"
)

func TestBuildReleasesOwnedResourcesOnFailureAndBeforeRun(t *testing.T) {
	for _, failed := range []bool{true, false} {
		name := "close-before-run"
		if failed {
			name = "failed-generation"
		}
		t.Run(name, func(t *testing.T) {
			installation, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Join(t.TempDir(), "desktop")})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(installation.Paths.Config)
			if err != nil {
				t.Fatal(err)
			}
			options := Config{Path: installation.Paths.Config}
			if failed {
				options.Environment = &Environment{}
			}
			application, err := Build(t.Context(), options)
			if failed {
				if err == nil || !strings.Contains(err.Error(), "coordinated application needs its generation ledger") {
					if application != nil {
						_ = application.Close()
					}
					t.Fatalf("missing generation ledger: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = application.Close() })
				if err := application.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// A second owner can open the state directory only after every completed
			// construction step has released its process-wide resources.
			release, err := runtime.AcquireLock(filepath.Dir(cfg.Gateway.StatePath))
			if err != nil {
				t.Fatalf("application left its state directory locked: %v", err)
			}
			release()
		})
	}
}
