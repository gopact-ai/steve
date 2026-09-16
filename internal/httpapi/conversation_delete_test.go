package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
)

type fakeDeleteAdmin struct {
	consoleapi.Admin
	deleted []string
	err     error
}

func (a *fakeDeleteAdmin) DeleteConversation(_ context.Context, conversation string) error {
	a.deleted = append(a.deleted, conversation)
	return a.err
}

func TestDeleteConversationReachesTheAdmin(t *testing.T) {
	admin := &fakeDeleteAdmin{}
	server := taskMetaServer(t, admin)

	status, _, _ := taskMetaRequest(t, server, http.MethodDelete, "/console/conversations/console%3Aone", "owner", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d; want 200", status)
	}
	if len(admin.deleted) != 1 || admin.deleted[0] != "console:one" {
		t.Fatalf("deleted = %v", admin.deleted)
	}

	status, _, _ = taskMetaRequest(t, server, http.MethodDelete, "/console/conversations/console%3Aone", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delete status = %d; want 401", status)
	}

	admin.err = errors.New("会话还有没跑完的回合")
	status, _, body := taskMetaRequest(t, server, http.MethodDelete, "/console/conversations/console%3Aone", "owner", "")
	if status != http.StatusBadRequest {
		t.Fatalf("refused delete status = %d; want 400", status)
	}
	if len(body) == 0 {
		t.Fatal("a refusal must say why")
	}
}
