package artifact

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// CheckDeclarationsTx keeps ownership stable while an admitted landing WAL
// can still write. Only artifact interprets its own operation data.
func CheckDeclarationsTx(tx *ledger.Tx, desired []project.Project) error {
	operations, err := tx.Operations(landKind, "")
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.State != LandApplying && op.State != LandRecoveryPending {
			continue
		}
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			return fmt.Errorf("landing %s: %w", op.ID, err)
		}
		if land.Target.Path == "" {
			return fmt.Errorf("landing %s has no recorded recovery target", op.ID)
		}
		if land.Source != nil && (land.Source.Execution == nil || land.Source.Execution.TaskID == "" || land.Source.AttemptID == "") {
			return fmt.Errorf("landing %s has incomplete source identity", op.ID)
		}
		retained := false
		for _, p := range desired {
			if p.ID == land.Project && p.Home.Node == land.Target.Node && filepath.Clean(p.Home.Path) == filepath.Clean(land.Target.Path) {
				retained = true
			}
			for _, ws := range p.Workspaces() {
				if ws.Node != land.Target.Node {
					continue
				}
				a, b := filepath.Clean(ws.Path), filepath.Clean(land.Target.Path)
				overlap := a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
				if overlap && (p.ID != land.Project || ws.Kind != project.KindCanonical || a != b) {
					return fmt.Errorf("landing %s still owns %s on %s; cannot assign it to project %s", op.ID, b, ws.Node, p.ID)
				}
			}
		}
		if !retained {
			return fmt.Errorf("landing %s must recover on %s before project %s can move or retire", op.ID, land.Target.Path, land.Project)
		}
	}
	return nil
}
