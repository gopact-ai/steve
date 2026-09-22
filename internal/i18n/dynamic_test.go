package i18n

import "testing"

func TestDynamicCatalogReadsLocaleWithoutReplacingCatalog(t *testing.T) {
	locale := LocaleZH
	reads := 0
	catalog := Dynamic(func() Locale {
		reads++
		return locale
	})
	copied := catalog
	if catalog.IsZero() || reads != 0 {
		t.Fatal("a dynamic catalog is configured without sampling its source")
	}
	for _, next := range []Locale{LocaleZH, LocaleEN, "", "unknown", LocaleZH} {
		locale = next
		before := reads
		if got, want := copied.Locale(), New(next).Locale(); got != want || reads != before+1 {
			t.Fatalf("Locale() = %q, want %q; reads=%d", got, want, reads-before)
		}
		before = reads
		if got, want := catalog.T(Switched, "agent"), New(next).T(Switched, "agent"); got != want || reads != before+1 {
			t.Fatalf("T() = %q, want %q; reads=%d", got, want, reads-before)
		}
	}
}

func TestCatalogZeroAndStaticLocale(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog Catalog
		zero    bool
		locale  Locale
	}{
		{"zero", Catalog{}, true, LocaleZH},
		{"nil source", Dynamic(nil), true, LocaleZH},
		{"empty static", New(""), false, LocaleZH},
		{"invalid static", New("unknown"), false, LocaleZH},
		{"english", New(LocaleEN), false, LocaleEN},
		{"empty dynamic", Dynamic(func() Locale { return "" }), false, LocaleZH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.catalog.IsZero() != tc.zero || tc.catalog.Locale() != tc.locale {
				t.Fatalf("IsZero=%v Locale=%q", tc.catalog.IsZero(), tc.catalog.Locale())
			}
		})
	}
}
