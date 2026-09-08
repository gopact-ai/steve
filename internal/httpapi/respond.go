package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// writeJSON encodes a response body. By the time the body is written the
// status has gone out, so a failure cannot change the answer; it is still
// logged, because it is either a client that left mid-write or a value
// that does not marshal, and the second is a bug worth seeing.
func writeJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn(fmt.Sprintf("httpapi: encode response: %v", err))
	}
}
