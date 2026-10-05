package project

import "github.com/gopact-ai/steve/internal/ledger"

// DiscardConversationBindingTx removes only a permanently discarded
// conversation's binding and its CAS name. Project and workspace history stay.
func DiscardConversationBindingTx(tx *ledger.Tx, conversationID string) error {
	if _, err := tx.Exec("DELETE FROM bindings WHERE kind=? AND id=?", kindBinding, conversationID); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM names WHERE name=?", bindingNameBase+conversationID+"/project")
	return err
}
