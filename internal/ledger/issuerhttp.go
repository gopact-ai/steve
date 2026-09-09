package ledger

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// IssuerHandler serves this ledger's leases to other regions' hubs. Every
// call carries the region's bearer token; the answers are the same tuples
// and the same refusals the local calls get.
func IssuerHandler(l *Ledger, token string) http.Handler {
	mux := http.NewServeMux()
	authed := func(next func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			next(w, r)
		}
	}
	type acquireReq struct {
		Key    string        `json:"key"`
		Holder string        `json:"holder"`
		TTL    time.Duration `json:"ttl"`
	}
	type leaseReq struct {
		Lease Lease         `json:"lease"`
		TTL   time.Duration `json:"ttl"`
	}
	type keyReq struct {
		Key string `json:"key"`
	}
	respond := func(w http.ResponseWriter, lease Lease, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			code := http.StatusConflict
			if errors.Is(err, ErrStale) {
				code = http.StatusPreconditionFailed
			}
			w.WriteHeader(code)
			// A body that cannot be written means the client has gone; the
			// refusal stands in the ledger whether or not it reads it.
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "kind": kindOf(err)})
			return
		}
		lease.Region = l.Region()
		// Likewise: the lease is already committed, and a client that
		// missed it renews or reacquires on its own.
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}
	mux.HandleFunc("/leases/acquire", authed(func(w http.ResponseWriter, r *http.Request) {
		var req acquireReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lease, err := l.Acquire(r.Context(), req.Key, req.Holder, req.TTL)
		respond(w, lease, err)
	}))
	mux.HandleFunc("/leases/renew", authed(func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lease, err := l.Renew(r.Context(), req.Lease, req.TTL)
		respond(w, lease, err)
	}))
	mux.HandleFunc("/leases/release", authed(func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respond(w, req.Lease, l.Release(r.Context(), req.Lease))
	}))
	mux.HandleFunc("/leases/check", authed(func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respond(w, req.Lease, l.Check(r.Context(), req.Lease))
	}))
	mux.HandleFunc("/leases/invalidate", authed(func(w http.ResponseWriter, r *http.Request) {
		var req keyReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respond(w, Lease{Key: req.Key}, l.Invalidate(r.Context(), req.Key))
	}))
	return mux
}

func kindOf(err error) string {
	switch {
	case errors.Is(err, ErrStale):
		return "stale"
	case errors.Is(err, ErrHeld):
		return "held"
	default:
		return "error"
	}
}

// HTTPIssuer reaches another region's hub.
type HTTPIssuer struct {
	URL    string
	Token  string
	Client *http.Client
}

func NewHTTPIssuer(url, token string) *HTTPIssuer {
	return &HTTPIssuer{URL: strings.TrimRight(url, "/"), Token: token, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTPIssuer) call(ctx context.Context, path string, body any) (Lease, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Lease{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+path, bytes.NewReader(raw))
	if err != nil {
		return Lease{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return Lease{}, fmt.Errorf("region issuer %s: %w", h.URL, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Lease Lease  `json:"lease"`
		Error string `json:"error"`
		Kind  string `json:"kind"`
	}
	// An error body may be plain text from http.Error; a success body must
	// carry the lease.
	if err := json.Unmarshal(payload, &out); err != nil && resp.StatusCode == http.StatusOK {
		return Lease{}, fmt.Errorf("region issuer %s: %w", h.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(payload))
		}
		switch out.Kind {
		case "stale":
			return Lease{}, fmt.Errorf("%w: %s", ErrStale, msg)
		case "held":
			return Lease{}, fmt.Errorf("%w: %s", ErrHeld, msg)
		}
		return Lease{}, fmt.Errorf("region issuer %s: %s", h.URL, msg)
	}
	return out.Lease, nil
}

func (h *HTTPIssuer) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error) {
	return h.call(ctx, "/leases/acquire", map[string]any{"key": key, "holder": holder, "ttl": ttl})
}

func (h *HTTPIssuer) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	return h.call(ctx, "/leases/renew", map[string]any{"lease": lease, "ttl": ttl})
}

func (h *HTTPIssuer) Release(ctx context.Context, lease Lease) error {
	_, err := h.call(ctx, "/leases/release", map[string]any{"lease": lease})
	return err
}

func (h *HTTPIssuer) Check(ctx context.Context, lease Lease) error {
	_, err := h.call(ctx, "/leases/check", map[string]any{"lease": lease})
	return err
}

func (h *HTTPIssuer) Invalidate(ctx context.Context, key string) error {
	_, err := h.call(ctx, "/leases/invalidate", map[string]any{"key": key})
	return err
}
