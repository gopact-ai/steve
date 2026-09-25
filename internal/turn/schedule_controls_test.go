package turn

import "testing"

func TestScheduleCommandsAreImmediateControls(t *testing.T) {
	for _, input := range []string{
		"/every 30m check CI", "/at tomorrow 09:00 report",
		"/schedules", "/schedules cancel 1", "/schedules retry 1",
		"@codex /schedules confirm 1", "@codex /every 1h check",
	} {
		if _, parsed := ParseAddressedInput(input); !parsed.Immediate() {
			t.Errorf("schedule control must not wait for an agent or become a quoted prompt: %q", input)
		}
	}
}
