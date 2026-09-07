package home

import (
	"errors"
	"strings"
	"testing"
)

func TestReaderLoadsOneSharedViewAndKeepsGuestBoundaries(t *testing.T) {
	reads := 0
	reader := Reader{Label: "shared profile", Locale: LocaleEN, ReadFiles: func() (map[string]string, error) {
		reads++
		return map[string]string{FileSoul: "shared soul", FileUser: "private user", FileMemory: "private memory"}, nil
	}}
	owner, err := reader.Load(ModeOwner)
	if err != nil || reads != 1 || owner.Soul != "shared soul" || owner.User != "private user" || owner.Memory != "private memory" {
		t.Fatalf("owner snapshot: %+v %v reads=%d", owner, err, reads)
	}
	if strings.Contains(owner.Identity, "write MEMORY.md") || !strings.Contains(owner.Identity, "shared") {
		t.Fatal("shared identity points agents at a local file")
	}
	guest, err := reader.Load(ModeGuest)
	if err != nil || guest.User != "" || guest.Memory != "" || strings.Contains(guest.Prompt, "private user") || strings.Contains(guest.Prompt, "private memory") || !strings.Contains(guest.Prompt, "shared soul") {
		t.Fatalf("guest snapshot: %+v %v", guest, err)
	}
	reader.ReadFiles = func() (map[string]string, error) { return map[string]string{FileSoul: "soul"}, nil }
	if _, err := reader.Load(ModeGuest); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Load(ModeOwner); !errors.Is(err, ErrMissing) {
		t.Fatalf("owner accepted missing shared files: %v", err)
	}
}
