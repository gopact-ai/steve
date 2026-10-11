package identity

import (
	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

const AuthorityKind = "identity-authority"
const ExecutionKind = "identity-execution"
const TaskAuthorityKind = "identity-task-authority"

type ExecutionProof struct {
	Attempt   string  `json:"attempt"`
	Account   Account `json:"account"`
	Principal string  `json:"principal"`
	Revision  uint64  `json:"revision"`
}
type TaskAuthority struct {
	Attempt string          `json:"attempt"`
	Epoch   uint64          `json:"epoch"`
	Caller  *ExecutionProof `json:"caller,omitempty"`
}

func BindExecutionTx(tx *ledger.Tx, attempt string, account Account) error {
	binding, err := ResolveAccount(tx, account)
	if err != nil {
		return err
	}
	proof := ExecutionProof{Attempt: attempt, Account: account, Principal: binding.Principal, Revision: binding.Revision}
	old, found, err := durable.Get[ExecutionProof](tx, ExecutionKind, attempt)
	if err != nil {
		return err
	}
	if found {
		if old != proof {
			return ErrDenied
		}
		return nil
	}
	return tx.PutBinding(ExecutionKind, attempt, proof)
}
func CheckExecutionTx(r ledger.Reader, attempt string) (ExecutionProof, error) {
	proof, found, err := durable.Get[ExecutionProof](r, ExecutionKind, attempt)
	if err != nil {
		return proof, err
	}
	if !found || proof.Attempt != attempt {
		return proof, ErrDenied
	}
	binding, err := ResolveAccount(r, proof.Account)
	if err != nil {
		return proof, err
	}
	if binding.Principal != proof.Principal || binding.Revision != proof.Revision {
		return proof, ErrDenied
	}
	return proof, nil
}

// PinTaskTx retains the creating caller's exact execution proof for auxiliary
// plan work. Later callers of the same task cannot replace its provenance.
func PinTaskTx(tx *ledger.Tx, task, attempt string, epoch uint64) error {
	old, found, err := durable.Get[TaskAuthority](tx, TaskAuthorityKind, task)
	if err != nil {
		return err
	}
	if found && (old.Attempt != "" || old.Caller != nil) && old.Epoch == epoch {
		return nil
	}
	if _, err := CheckExecutionTx(tx, attempt); err != nil {
		return err
	}
	return tx.PutBinding(TaskAuthorityKind, task, TaskAuthority{Attempt: attempt, Epoch: epoch})
}
