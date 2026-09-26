package i18n

import (
	"context"

	"golang.org/x/text/language"
)

type localeContextKey struct{}

func WithLocale(ctx context.Context, locale Locale) context.Context {
	return context.WithValue(ctx, localeContextKey{}, locale)
}

func ContextLocale(ctx context.Context) Locale {
	locale, _ := ctx.Value(localeContextKey{}).(Locale)
	return locale
}

// LocaleFromHeader honors the browser's ordered language preferences. An
// unsupported or malformed header leaves the configured default in charge.
func LocaleFromHeader(header string) Locale {
	tags, _, err := language.ParseAcceptLanguage(header)
	if err != nil {
		return ""
	}
	for _, tag := range tags {
		base, _ := tag.Base()
		switch base.String() {
		case string(LocaleZH):
			return LocaleZH
		case string(LocaleEN):
			return LocaleEN
		}
	}
	return ""
}

// For is c in the language ctx carries, and c itself when ctx carries
// none: whoever holds the Hub's catalog answers a person who named no
// language in the Hub's.
func (c Catalog) For(ctx context.Context) Catalog {
	switch locale := ContextLocale(ctx); locale {
	case LocaleZH, LocaleEN:
		return New(locale)
	}
	return c
}

// FromContext is the catalog in the language ctx carries. Every entry — a
// console request, a turn, an agent tool call, startup and doctor — puts
// one there, the Hub's when the person named none.
func FromContext(ctx context.Context) Catalog { return Catalog{}.For(ctx) }
