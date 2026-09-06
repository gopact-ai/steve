package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/i18n"
)

func (c *Coordinator) localized(locale i18n.Locale) *Coordinator {
	if locale != i18n.LocaleZH && locale != i18n.LocaleEN {
		return c
	}
	return &Coordinator{coordinatorState: c.coordinatorState, text: i18n.New(locale)}
}

// VerbsFor localizes system labels without changing the coordinator used by
// another conversation or by a background channel.
func (c *Coordinator) VerbsFor(ctx context.Context) []Verb {
	return c.localized(i18n.ContextLocale(ctx)).Verbs()
}
