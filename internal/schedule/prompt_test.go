package schedule

import (
	"testing"
	"time"
)

func TestSchedulePreservesMultilineInstruction(t *testing.T) {
	prompt := "Check CI:\n\n- keep  two spaces\n- run `printf 'a  b'`\nReport only failures."
	for _, prefix := range []string{"30m", "mon 09:00"} {
		_, got, err := ParseEvery(prefix+" \t"+prompt, time.Now())
		if err != nil || got != prompt {
			t.Errorf("ParseEvery(%q): prompt=%q err=%v", prefix, got, err)
		}
	}
	_, got, err := ParseAt("tomorrow 09:00 "+prompt, time.Now())
	if err != nil || got != prompt {
		t.Fatalf("ParseAt: prompt=%q err=%v", got, err)
	}
}
