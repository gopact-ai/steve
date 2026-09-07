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
