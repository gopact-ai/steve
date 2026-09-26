package i18n

import (
	"errors"
	"strings"
	"testing"
)

// An error made from a template that wraps its cause stays findable with
// errors.Is in every language, and its sentence is fully formatted.
func TestErrorfWrapsTheCauseInEveryLanguage(t *testing.T) {
	cause := errors.New("state changed")
	for _, locale := range []Locale{LocaleZH, LocaleEN} {
		err := New(locale).Errorf(ClusterPlanChangedReview, cause)
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "%!") || !strings.HasPrefix(err.Error(), "state changed: ") {
			t.Errorf("%s: %q (wraps the cause: %v), want the cause wrapped and followed by the reason", locale, err, errors.Is(err, cause))
		}
	}
}
