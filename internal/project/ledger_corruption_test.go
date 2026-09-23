package project

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/datalevel"
)

func corruptProjectRow(t *testing.T, s *Store, table, kind, id, data string) {
	t.Helper()
	query := "UPDATE bindings SET data = ? WHERE kind = ? AND id = ?"
	if table == "operations" {
		query = "UPDATE operations SET data = ? WHERE kind = ? AND id = ?"
	}
	result, err := s.l.DB().Exec(query, data, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("corrupt %s %s/%s: affected=%d err=%v", table, kind, id, n, err)
	}
}

func checkProjectCorruption(t *testing.T, err error, kind string, ids ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("corrupt ledger rows were reported as a healthy list")
	}
	for _, text := range append([]string{kind}, ids...) {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("error %q does not locate %q", err, text)
		}
	}
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	if !errors.As(err, &syntax) || !errors.As(err, &wrongType) {
		t.Errorf("error does not preserve both JSON causes: %v", err)
	}
	if strings.Contains(err.Error(), "private-payload") {
		t.Errorf("error exposes row contents: %v", err)
	}
}

func TestProjectListReportsCorruptionWithValidRows(t *testing.T) {
	s := openStore(t)
	for _, id := range []string{"zulu", "bad-syntax", "bad-type", "alpha"} {
		if err := s.Declare(t.Context(), []Project{{ID: id, Home: Home{Path: t.TempDir()}}}); err != nil {
			t.Fatal(err)
		}
	}
	corruptProjectRow(t, s, "bindings", kindProject, "bad-syntax", `{"id":"different-id","private":"private-payload"`)
	corruptProjectRow(t, s, "bindings", kindProject, "bad-type", `{"id":"different-id","home":["private-payload"]}`)
	got, err := s.List(t.Context())
	checkProjectCorruption(t, err, kindProject, "bad-syntax", "bad-type")
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []string{"alpha", "zulu"}) {
		t.Fatalf("valid projects = %v; want [alpha zulu]", ids)
	}
}

func TestProjectListKeepsDeclarationGate(t *testing.T) {
	s := openStore(t)
	if err := s.Declare(t.Context(), []Project{{ID: "p", Home: Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	s.RequireDeclaration("not-applied")
	if got, err := s.List(t.Context()); len(got) != 0 || !errors.Is(err, ErrDeclarationPending) {
		t.Fatalf("pending declaration exposed live projects: %+v, %v", got, err)
	}
}

func TestProjectOperationsRejectCorruptProjectSet(t *testing.T) {
	tests := map[string]func(*Store, Project) error{
		"declare": func(s *Store, p Project) error {
			p.Level = datalevel.Restricted
			return s.Declare(t.Context(), []Project{p})
		},
		"set-copy": func(s *Store, p Project) error {
			_, err := s.SetCopy(t.Context(), p.ID, Copy{Node: "new-node", Path: "/new-copy"})
			return err
		},
		"delete-copy": func(s *Store, p Project) error {
			return s.DeleteCopy(t.Context(), p.ID, "node")
		},
		"retire": func(s *Store, p Project) error {
			return s.Retire(t.Context(), p.ID)
		},
		"conflict": func(s *Store, p Project) error {
			_, _, err := s.Conflict(t.Context(), p.Home.Node, p.Home.Path)
			return err
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			p := Project{ID: "good", Home: Home{Path: t.TempDir()}, Copies: map[string]Copy{"node": {Path: "/copy"}}}
			if err := s.Declare(t.Context(), []Project{p, {ID: "bad", Home: Home{Path: t.TempDir()}}}); err != nil {
				t.Fatal(err)
			}
			corruptProjectRow(t, s, "bindings", kindProject, "bad", `{`)
			before, err := s.l.Bindings(t.Context(), kindProject)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(s, p); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("%s trusted partial project metadata: %v", name, err)
			}
			after, err := s.l.Bindings(t.Context(), kindProject)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected %s changed project metadata: %v", name, err)
			}
		})
	}
}

func TestGrantsReportCorruptionWithValidRows(t *testing.T) {
	s := openStore(t)
	for _, row := range []struct {
		kind string
		id   string
		g    Grant
	}{
		{kindGrant, "p/zulu", Grant{Project: "p", Principal: "zulu", Role: RoleWrite}},
		{kindGrant, "p/alpha", Grant{Project: "p", Principal: "alpha", Role: RoleWrite}},
		{kindConfigGrant, "p/alpha", Grant{Project: "p", Principal: "alpha", Role: RoleRead}},
		{kindGrant, "other/beta", Grant{Project: "other", Principal: "beta", Role: RoleWrite}},
		{kindGrant, "bad-syntax", Grant{}},
		{kindConfigGrant, "bad-type", Grant{}},
	} {
		if err := s.l.PutBinding(t.Context(), row.kind, row.id, row.g); err != nil {
			t.Fatal(err)
		}
	}
	corruptProjectRow(t, s, "bindings", kindGrant, "bad-syntax", `{"private":"private-payload"`)
	corruptProjectRow(t, s, "bindings", kindConfigGrant, "bad-type", `{"principal":["private-payload"]}`)
	for _, projectID := range []string{"", "p", "missing"} {
		got, err := s.Grants(t.Context(), projectID)
		checkProjectCorruption(t, err, kindGrant, "bad-syntax", "bad-type", kindConfigGrant)
		var principals []string
		for _, g := range got {
			principals = append(principals, g.Principal)
			if g.Principal == "alpha" && g.Role != RoleRead {
				t.Errorf("configured grant did not override manual grant: %+v", g)
			}
		}
		var want []string
		switch projectID {
		case "":
			want = []string{"alpha", "beta", "zulu"}
		case "p":
			want = []string{"alpha", "zulu"}
		}
		if !slices.Equal(principals, want) {
			t.Errorf("grants(%q) = %v; want %v", projectID, principals, want)
		}
	}
}

func TestGrantsDoNotHideOverriddenCorruptionOrFallBack(t *testing.T) {
	for _, corruptKind := range []string{kindGrant, kindConfigGrant} {
		t.Run(corruptKind, func(t *testing.T) {
			s := openStore(t)
			const id = "p/principal"
			for _, kind := range []string{kindGrant, kindConfigGrant} {
				if err := s.l.PutBinding(t.Context(), kind, id, Grant{Project: "p", Principal: "principal", Role: RoleWrite}); err != nil {
					t.Fatal(err)
				}
			}
			corruptProjectRow(t, s, "bindings", corruptKind, id, `{`)
			got, err := s.Grants(t.Context(), "p")
			if err == nil || !strings.Contains(err.Error(), corruptKind) || !strings.Contains(err.Error(), id) {
				t.Fatalf("grant corruption not located: %+v, %v", got, err)
			}
			want := 1
			if corruptKind == kindConfigGrant {
				want = 0
			}
			if len(got) != want {
				t.Fatalf("corrupt %s: grants=%+v; want %d effective records", corruptKind, got, want)
			}
		})
	}
}

func TestAccessRejectsCorruptGrant(t *testing.T) {
	for _, corruptKind := range []string{kindGrant, kindConfigGrant} {
		t.Run(corruptKind, func(t *testing.T) {
			s := openStore(t)
			if err := s.Declare(t.Context(), []Project{{ID: "p", Home: Home{Path: t.TempDir()}}}); err != nil {
				t.Fatal(err)
			}
			const id = "p/principal"
			g := Grant{Project: "p", Principal: "principal", Role: RoleWrite}
			if err := s.l.PutBinding(t.Context(), kindGrant, id, g); err != nil {
				t.Fatal(err)
			}
			if err := s.l.PutBinding(t.Context(), corruptKind, id, g); err != nil {
				t.Fatal(err)
			}
			corruptProjectRow(t, s, "bindings", corruptKind, id, `{"role":"admin","at":false}`)
			if role, err := s.Access(t.Context(), "p", "principal", "owner"); role != RoleNone || err == nil {
				t.Fatalf("corrupt grant authorized access: role=%s err=%v", role, err)
			}
		})
	}
}

func TestPendingDisclosuresReportCorruptionWithValidRows(t *testing.T) {
	s := openStore(t)
	for i, id := range []string{"new", "bad-syntax", "resolved", "bad-type", "old"} {
		state := DisclosureProposed
		if id == "resolved" {
			state = DisclosureApproved
		}
		req := DisclosureRequest{ID: id, ProposedAt: time.Unix(int64(5-i), 0)}
		if _, err := s.l.Begin(t.Context(), id, kindDisclosureOp, state, "test", req); err != nil {
			t.Fatal(err)
		}
	}
	corruptProjectRow(t, s, "operations", kindDisclosureOp, "bad-syntax", `{"private":"private-payload"`)
	corruptProjectRow(t, s, "operations", kindDisclosureOp, "bad-type", `{"bytes":"private-payload"}`)
	got, err := s.PendingDisclosures(t.Context())
	checkProjectCorruption(t, err, kindDisclosureOp, "bad-syntax", "bad-type")
	var ids []string
	for _, req := range got {
		ids = append(ids, req.ID)
	}
	if !slices.Equal(ids, []string{"old", "new"}) {
		t.Fatalf("valid pending disclosures = %v; want [old new]", ids)
	}
}

func TestResolveDisclosureRejectsCorruptRequest(t *testing.T) {
	s := openStore(t)
	if err := s.ProposeDisclosure(t.Context(), DisclosureRequest{ID: "bad"}); err != nil {
		t.Fatal(err)
	}
	corruptProjectRow(t, s, "operations", kindDisclosureOp, "bad", `{`)
	if err := s.ResolveDisclosure(t.Context(), "bad", true, "owner"); err == nil {
		t.Fatal("corrupt disclosure was approved")
	}
	op, found, err := s.l.Operation(t.Context(), "bad")
	if err != nil || !found || op.State != DisclosureProposed || op.Revision != 1 {
		t.Fatalf("rejected disclosure changed operation: %+v, %v", op, err)
	}
}
