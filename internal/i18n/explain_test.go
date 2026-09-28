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

// An explained error reads as its reader's language says it and still
// holds what it wraps; explaining it again says that afresh.
func TestExplainedSaysAndStillWraps(t *testing.T) {
	zh, en := New(LocaleZH), New(LocaleEN)
	err := en.Explained(fmt.Errorf("upload: %w", refusal{}))
	if got := err.Error(); got != en.T(NodeBinaryUnreadable) {
		t.Fatalf("explained = %q", got)
	}
	var said refusal
	if !errors.As(err, &said) {
		t.Fatalf("explained %v no longer wraps its refusal", err)
	}
	if got := zh.Explain(err); got != zh.T(NodeBinaryUnreadable) {
		t.Fatalf("explained again = %q", got)
	}
}
