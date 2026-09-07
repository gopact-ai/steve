package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const RPCPath = "/coordination/v1/"

type RPCOptions struct {
	// AuthorizeControl validates an owner authorization carried by an already
	// authenticated peer. It must return the trusted audit actor; request body
	// actors are ignored. Nil denies every administrative mutation.
	AuthorizeControl func(*http.Request, Identity, string) (string, error)
	MaxBodyBytes     int64
}

type rpcFailure struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	LeaderID string   `json:"leader_id,omitempty"`
	Members  []Member `json:"members,omitempty"`
}

type rpcHandler struct {
	service *Service
	options RPCOptions
}

func NewRPCHandler(service *Service, options RPCOptions) http.Handler {
	if options.MaxBodyBytes <= 0 {
		options.MaxBodyBytes = 32 << 20
	}
	return &rpcHandler{service: service, options: options}
}

func (h *rpcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	identity, err := requestIdentity(r, h.service.config.ClusterID)
	if err != nil {
		h.failure(w, http.StatusUnauthorized, err)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, RPCPath)
	if action == r.URL.Path || strings.Contains(action, "/") {
		http.NotFound(w, r)
		return
	}
	if action == "status" || action == "state" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.failure(w, http.StatusMethodNotAllowed, ErrInvalid)
			return
		}
		if action == "status" {
			json.NewEncoder(w).Encode(h.service.Status())
			return
		}
		state, err := h.service.ReadState(r.Context())
		h.reply(w, state, err)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.failure(w, http.StatusMethodNotAllowed, ErrInvalid)
		return
	}
	if action != "app" && action != "writer" && action != "transfer" && action != "policy" && action != "eligibility" && action != "join" && action != "remove" && action != "address" {
		http.NotFound(w, r)
		return
	}
	actor := ""
	if action != "app" && action != "writer" {
		if h.options.AuthorizeControl == nil {
			h.failure(w, http.StatusForbidden, fmt.Errorf("%w: owner authorization is required", ErrInvalid))
			return
		}
		actor, err = h.options.AuthorizeControl(r, identity, action)
		if err != nil || strings.TrimSpace(actor) == "" {
			h.failure(w, http.StatusForbidden, fmt.Errorf("%w: owner authorization denied", ErrInvalid))
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.options.MaxBodyBytes)
	defer r.Body.Close()
	decode := func(target any) bool {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			h.failure(w, http.StatusBadRequest, fmt.Errorf("%w: invalid command body", ErrInvalid))
			return false
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			h.failure(w, http.StatusBadRequest, fmt.Errorf("%w: command body must contain one object", ErrInvalid))
			return false
		}
		return true
	}
	var result Result
	switch action {
	case "writer":
		var request WriterRequest
		if !decode(&request) {
			return
		}
		if request.CallerNodeID != identity.NodeID {
			h.failure(w, http.StatusForbidden, ErrNotCoordinator)
			return
		}
		request.CallerNodeID = identity.NodeID
		result, err = h.service.BeginWriter(r.Context(), request)
	case "app":
		var request AppCommand
		if !decode(&request) {
			return
		}
		if request.CallerNodeID != identity.NodeID {
			h.failure(w, http.StatusForbidden, ErrNotCoordinator)
			return
		}
		request.CallerNodeID = identity.NodeID
		result, err = h.service.ApplyApp(r.Context(), request)
	case "transfer":
		var request TransferRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.Transfer(r.Context(), request)
	case "policy":
		var request PolicyRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.SetAutoFailover(r.Context(), request)
	case "eligibility":
		var request EligibilityRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.SetEligibility(r.Context(), request)
	case "join":
		var request JoinRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.Join(r.Context(), request)
	case "remove":
		var request RemoveRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.Remove(r.Context(), request)
	case "address":
		var request MemberAddressRequest
		if !decode(&request) {
			return
		}
		request.Actor = actor
		result, err = h.service.UpdateMemberAddress(r.Context(), request)
	}
	h.reply(w, result, err)
}

func requestIdentity(r *http.Request, clusterID string) (Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return Identity{}, fmt.Errorf("%w: verified mutual TLS is required", ErrInvalid)
	}
	identity, err := CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return Identity{}, err
	}
	if identity.ClusterID != clusterID {
		return Identity{}, fmt.Errorf("%w: peer belongs to another cluster", ErrInvalid)
	}
	return identity, nil
}

func (h *rpcHandler) reply(w http.ResponseWriter, value any, err error) {
	if err == nil {
		json.NewEncoder(w).Encode(value)
		return
	}
	status := http.StatusConflict
	if errors.Is(err, ErrNotLeader) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrApplication) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		status = http.StatusServiceUnavailable
	}
	if errors.Is(err, ErrInvalid) {
		status = http.StatusBadRequest
	}
	h.failure(w, status, err)
}

func (h *rpcHandler) failure(w http.ResponseWriter, status int, err error) {
	failure := rpcFailure{Code: errorCode(err), Message: err.Error()}
	if status == http.StatusServiceUnavailable {
		state := h.service.Status()
		failure.LeaderID = state.LeaderID
		for _, member := range state.Members {
			failure.Members = append(failure.Members, member)
		}
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(failure)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotLeader):
		return "not_leader"
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "unavailable"
	case errors.Is(err, ErrNotReady):
		return "not_ready"
	case errors.Is(err, ErrStaleEpoch):
		return "stale_epoch"
	case errors.Is(err, ErrStaleWriter):
		return "stale_writer"
	case errors.Is(err, ErrNotCoordinator):
		return "not_coordinator"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrCommandConflict):
		return "command_conflict"
	case errors.Is(err, ErrApplication):
		return "application"
	default:
		return "invalid"
	}
}

func (f rpcFailure) err() error {
	var kind error
	switch f.Code {
	case "not_leader":
		kind = ErrNotLeader
	case "unavailable":
		kind = ErrUnavailable
	case "not_ready":
		kind = ErrNotReady
	case "stale_epoch":
		kind = ErrStaleEpoch
	case "stale_writer":
		kind = ErrStaleWriter
	case "not_coordinator":
		kind = ErrNotCoordinator
	case "conflict":
		kind = ErrConflict
	case "command_conflict":
		kind = ErrCommandConflict
	case "application":
		kind = ErrApplication
	default:
		kind = ErrInvalid
	}
	return fmt.Errorf("%w: %s", kind, f.Message)
}
