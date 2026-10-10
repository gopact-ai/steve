package acphost

import (
	"context"
	"errors"

	"github.com/gopact-ai/acp"
)

// ErrCloseUnconfirmed means a close has no matched protocol response. The
// original process may still own the session, so another RPC is not a retry.
var ErrCloseUnconfirmed = errors.New("session close outcome is unconfirmed")

// CloseSession releases a protocol session when supported, otherwise only its
// local bookkeeping. Pending/unknown lifecycle operations exclude one another.
// Only the original owned process's positive stop permits native retirement of
// an unknown operation; that is not a protocol ACK or provider deletion proof.
// This method never stops a shared host to close one of its sessions.
func (h *Host) CloseSession(ctx context.Context, sid acp.SessionID) error {
	return h.closeSession(ctx, sid)
}
