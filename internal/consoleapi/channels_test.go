package consoleapi

import (
	"encoding/json"
	"testing"
	"time"
)

// A reconnect names when its last attempt failed once one has, and leaves
// it out before: the console tells a client that stopped trying by it.
func TestChannelReconnectNamesItsLastAttemptOnceOneFailed(t *testing.T) {
	since := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		reconnect ChannelReconnect
		want      string
	}{
		{name: "lost", reconnect: ChannelReconnect{Since: since}, want: `{"since":"2026-09-26T10:00:00Z","attempts":0}`},
		{
			name:      "failed",
			reconnect: ChannelReconnect{Since: since, Attempts: 2, LastAttemptAt: since.Add(4 * time.Minute), LastError: "503: system busy"},
			want:      `{"since":"2026-09-26T10:00:00Z","attempts":2,"last_attempt_at":"2026-09-26T10:04:00Z","last_error":"503: system busy"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.reconnect)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("json.Marshal() = %s, %v; want %s", raw, err, tc.want)
			}
		})
	}
}
