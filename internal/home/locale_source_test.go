package home

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHomeLoadSamplesLocaleOnceAndPreservesIdentity(t *testing.T) {
	path := t.TempDir()
	files := map[string]string{FileSoul: "custom soul", FileUser: "private user", FileMemory: "private memory"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readFiles := func() (map[string]string, error) { return files, nil }
	for _, kind := range []string{"directory", "reader"} {
		t.Run(kind, func(t *testing.T) {
			locale, reads := LocaleZH, 0
			source := func() Locale {
				reads++
				return locale
			}
			var dynamic Loader = Dir{Path: path, Locale: LocaleEN, LocaleSource: source}
			if kind == "reader" {
				dynamic = Reader{ReadFiles: readFiles, Label: "profile", Locale: LocaleEN, LocaleSource: source}
			}
			for _, next := range []Locale{LocaleZH, LocaleEN, "", LocaleZH} {
				locale = next
				var fixed Loader = Dir{Path: path, Locale: next}
				if kind == "reader" {
					fixed = Reader{ReadFiles: readFiles, Label: "profile", Locale: next}
				}
				for _, mode := range []Mode{ModeOwner, ModeGuest, ModeNone} {
					before := reads
					got, err := dynamic.Load(mode)
					if err != nil {
						t.Fatal(err)
					}
					want, err := fixed.Load(mode)
					if err != nil {
						t.Fatal(err)
					}
					if reads != before+1 || !reflect.DeepEqual(got, want) {
						t.Fatalf("locale=%q mode=%q reads=%d\ngot=%+v\nwant=%+v", next, mode, reads-before, got, want)
					}
				}
			}
		})
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(got) != want {
			t.Fatalf("locale change rewrote identity file %s: %q %v", name, got, err)
		}
	}
}

func TestReaderSamplesLocaleBeforeReadingIdentity(t *testing.T) {
	locale, reads := LocaleZH, 0
	files := map[string]string{FileSoul: "soul", FileUser: "user", FileMemory: "memory"}
	reader := Reader{
		LocaleSource: func() Locale { reads++; return locale },
		ReadFiles: func() (map[string]string, error) {
			locale = LocaleEN
			return files, nil
		},
	}
	for _, wantLocale := range []Locale{LocaleZH, LocaleEN} {
		before := reads
		got, err := reader.Load(ModeOwner)
		if err != nil {
			t.Fatal(err)
		}
		want, err := (Reader{Locale: wantLocale, ReadFiles: func() (map[string]string, error) { return files, nil }}).Load(ModeOwner)
		if err != nil || reads != before+1 || !reflect.DeepEqual(got, want) {
			t.Fatalf("load did not retain its entry locale %q: %+v %v reads=%d", wantLocale, got, err, reads-before)
		}
	}
}
