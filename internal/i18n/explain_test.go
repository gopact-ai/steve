package i18n

import (
	"errors"
	"fmt"
	"testing"
)

type refusal struct{}

func (refusal) Error() string           { return "refused" }
func (refusal) Say(text Catalog) string { return text.T(NodeBinaryUnreadable) }

// An error that can be said is said in the reader's language, even
// wrapped; any other error is read as it is.
func TestExplainSaysWhatCanBeSaid(t *testing.T) {
	zh := New(LocaleZH)
	if got := zh.Explain(fmt.Errorf("upload: %w", refusal{})); got != zh.T(NodeBinaryUnreadable) {
		t.Fatalf("wrapped refusal = %q", got)
	}
	if got := zh.Explain(errors.New("disk full")); got != "disk full" {
		t.Fatalf("plain error = %q", got)
	}
}
