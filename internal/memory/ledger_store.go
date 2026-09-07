package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
)

const (
	ledgerMemoryKind    = "memory.scope"
	ledgerIdentityKind  = "memory.identity"
	ledgerAuditHeadKind = "memory.audit.head"
)

type actorContextKey struct{}

func withActor(ctx context.Context, who Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, who)
}

func memoryActor(ctx context.Context) Actor {
	actor, _ := ctx.Value(actorContextKey{}).(Actor)
	return actor
}

type scopeRecord struct {
	Format   int                       `json:"format"`
	Text     string                    `json:"text"`
	Receipts map[string]requestReceipt `json:"receipts,omitempty"`
}

type identityRecord struct {
	Format       int               `json:"format"`
	Bootstrapped bool              `json:"bootstrapped"`
	Files        map[string]string `json:"files"`
}

// AuditRecord is a committed memory operation. It contains metadata and the
// receipt, never the fact or identity document's text.
type AuditRecord struct {
	Sequence uint64 `json:"sequence"`
	auditLine
}

// LedgerStore keeps memory, request receipts and audit metadata in replicated
// transactions. It has no cached document that can outlive a coordinator.
type LedgerStore struct {
	book  *ledger.Ledger
	guard func(context.Context, *ledger.Tx) error
}

func NewLedgerStore(book *ledger.Ledger) *LedgerStore { return &LedgerStore{book: book} }
func (*LedgerStore) Name() string                     { return "ledger" }

// SetWriteGuard installs the caller's execution check before this store is
// used. Validation and every memory mutation share the same transaction.
func (s *LedgerStore) SetWriteGuard(guard func(context.Context, *ledger.Tx) error) {
	s.guard = guard
}

func validateScope(scope Scope) error {
	if scope.Kind == KindGlobal && scope.Project == "" {
		return nil
	}
	if scope.Kind == KindProject && scope.Project != "" && !strings.ContainsAny(scope.Project, "/\\ \t\r\n") && !strings.HasPrefix(scope.Project, ".") {
		return nil
	}
	return fmt.Errorf("invalid memory scope %q", scope.String())
}

func (s *LedgerStore) update(ctx context.Context, change func(*ledger.Tx) error) error {
	if s.book == nil {
		return errors.New("memory ledger is not configured")
	}
	return s.book.Update(ctx, func(tx *ledger.Tx) error {
		if s.guard != nil {
			if err := s.guard(ctx, tx); err != nil {
				return err
			}
		}
		return change(tx)
	})
}

func binding(tx *ledger.Tx, kind, id string, out any) (bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), out)
}

func loadScope(tx *ledger.Tx, scope Scope) (scopeRecord, bool, error) {
	if err := validateScope(scope); err != nil {
		return scopeRecord{}, false, err
	}
	var record scopeRecord
	found, err := binding(tx, ledgerMemoryKind, scope.String(), &record)
	if err != nil {
		return scopeRecord{}, false, err
	}
	if found && record.Format != 1 {
		return scopeRecord{}, false, errors.New("unsupported memory record")
	}
	if !found {
		record.Format = 1
	}
	return record, found, nil
}

func (s *LedgerStore) readScope(ctx context.Context, scope Scope) (scopeRecord, error) {
	var record scopeRecord
	err := s.update(ctx, func(tx *ledger.Tx) error { var err error; record, _, err = loadScope(tx, scope); return err })
	return record, err
}

// CheckRead keeps an optional retrieval index behind the same authority check
// as a direct snapshot, including coordinator-generation and quorum checks.
func (s *LedgerStore) CheckRead(ctx context.Context, scope Scope) error {
	_, err := s.readScope(ctx, scope)
	return err
}

func (s *LedgerStore) Snapshot(ctx context.Context, scope Scope) (string, int, error) {
	record, err := s.readScope(ctx, scope)
	if err != nil {
		return "", 0, err
	}
	if (scope.Kind == KindProject || home.IsTemplate(record.Text)) && !hasFacts(record.Text) {
		return "", 0, nil
	}
	text := stripIDs(record.Text)
	return text, len([]byte(text)), nil
}

func (s *LedgerStore) Text(ctx context.Context, scope Scope) (string, error) {
	record, err := s.readScope(ctx, scope)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(record.Text) == "" {
		return Template(scope), nil
	}
	return stripIDs(record.Text), nil
}

func (s *LedgerStore) List(ctx context.Context, scope Scope) ([]Item, error) {
	record, err := s.readScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	return parse(scope, record.Text), nil
}

func (s *LedgerStore) Recall(ctx context.Context, scope Scope, query string, limit int) ([]Hit, error) {
	items, err := s.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	return rankMemory(items, query, limit), nil
}

func (s *LedgerStore) Remember(ctx context.Context, scope Scope, section, text string) (Receipt, error) {
	receipt, _, err := s.remember(ctx, scope, section, text, "", time.Now())
	return receipt, err
}

func (s *LedgerStore) RememberOnce(ctx context.Context, scope Scope, section, text, key string, now time.Time) (Receipt, bool, error) {
	if key == "" {
		return Receipt{}, false, errors.New("idempotency key is required")
	}
	return s.remember(ctx, scope, section, text, key, now)
}

func (s *LedgerStore) remember(ctx context.Context, scope Scope, section, text, key string, now time.Time) (Receipt, bool, error) {
	var receipt Receipt
	var replayed bool
	err := s.update(ctx, func(tx *ledger.Tx) error {
		record, _, err := loadScope(tx, scope)
		if err != nil {
			return err
		}
		if previous, ok := record.Receipts[key]; key != "" && ok && previous.At.Add(idempotencyTTL).After(now) {
			receipt, replayed = previous.Receipt, true
			return nil
		}
		after, got, err := prepareRemember(scope, section, text, record.Text)
		if err != nil {
			return err
		}
		receipt = got
		record.Text = after
		if key != "" {
			if record.Receipts == nil {
				record.Receipts = map[string]requestReceipt{}
			}
			for k, previous := range record.Receipts {
				if !previous.At.Add(idempotencyTTL).After(now) {
					delete(record.Receipts, k)
				}
			}
			record.Receipts[key] = requestReceipt{Scope: scope, Key: key, At: now, Receipt: receipt}
		}
		if receipt.New || key != "" {
			if err := tx.PutBinding(ledgerMemoryKind, scope.String(), record); err != nil {
				return err
			}
		}
		line := auditLine{At: now.UTC().Format(time.RFC3339), Op: "remember", Scope: scope, Actor: memoryActor(ctx), ID: receipt.ID, Section: section, Bytes: len([]byte(text)), New: receipt.New, IdempotencyKey: key}
		if key != "" {
			line.Receipt = &receipt
		}
		return appendLedgerAudit(tx, line)
	})
	if err != nil {
		return Receipt{}, false, err
	}
	return receipt, replayed, nil
}

func (s *LedgerStore) Replace(ctx context.Context, scope Scope, text string) error {
	if strings.Contains(text, "<!-- m:") {
		return errors.New("the text carries id markers; edit the plain text, ids are kept for you")
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		record, _, err := loadScope(tx, scope)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for _, item := range parse(scope, record.Text) {
			if !strings.HasPrefix(item.ID, "h") {
				ids[hashOf(item.Text)] = item.ID
			}
		}
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if item, ok := parseBullet(scope, "", line); ok {
				if id, found := ids[hashOf(item.Text)]; found {
					lines[i] = strings.TrimRight(line, " \t") + " " + idComment(id)
				}
			}
		}
		record.Text = strings.Join(lines, "\n")
		if len([]byte(record.Text)) > Budget(scope) {
			return fmt.Errorf("memory would be %d bytes; only %d reach the agent — shorten it", len([]byte(record.Text)), Budget(scope))
		}
		if err := tx.PutBinding(ledgerMemoryKind, scope.String(), record); err != nil {
			return err
		}
		return appendLedgerAudit(tx, auditLine{Op: "replace", Scope: scope, Actor: memoryActor(ctx), Bytes: len([]byte(text))})
	})
}

func (s *LedgerStore) Forget(ctx context.Context, scope Scope, id string) (Item, error) {
	var removed Item
	err := s.update(ctx, func(tx *ledger.Tx) error {
		record, _, err := loadScope(tx, scope)
		if err != nil {
			return err
		}
		for _, item := range parse(scope, record.Text) {
			if item.ID == id {
				removed = item
				break
			}
		}
		if removed.ID == "" {
			return fmt.Errorf("no memory %s in %s", id, scope)
		}
		var kept []string
		for _, line := range strings.Split(record.Text, "\n") {
			if item, ok := parseBullet(scope, "", line); ok && item.ID == id {
				continue
			}
			kept = append(kept, line)
		}
		record.Text = strings.Join(kept, "\n")
		if err := tx.PutBinding(ledgerMemoryKind, scope.String(), record); err != nil {
			return err
		}
		return appendLedgerAudit(tx, auditLine{Op: "forget", Scope: scope, Actor: memoryActor(ctx), ID: id})
	})
	if err != nil {
		return Item{}, err
	}
	return removed, nil
}

// Bootstrap imports an explicitly supplied scope only if it has no ledger
// record. Subsequent edits and deletions never consult the source again.
func (s *LedgerStore) Bootstrap(ctx context.Context, scope Scope, text string, who Actor) (bool, error) {
	var changed bool
	err := s.update(ctx, func(tx *ledger.Tx) error {
		_, exists, err := loadScope(tx, scope)
		if err != nil || exists {
			return err
		}
		if len([]byte(text)) > Budget(scope) {
			return errors.New("imported memory exceeds its scope budget")
		}
		if err := tx.PutBinding(ledgerMemoryKind, scope.String(), scopeRecord{Format: 1, Text: text}); err != nil {
			return err
		}
		changed = true
		return appendLedgerAudit(tx, auditLine{Op: "bootstrap", Scope: scope, Actor: who, Bytes: len([]byte(text))})
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func appendLedgerAudit(tx *ledger.Tx, line auditLine) error {
	if err := validateScope(line.Scope); err != nil {
		return err
	}
	var sequence uint64
	if _, err := binding(tx, ledgerAuditHeadKind, line.Scope.String(), &sequence); err != nil {
		return err
	}
	if sequence == ^uint64(0) {
		return errors.New("memory audit sequence exhausted")
	}
	sequence++
	if line.At == "" {
		line.At = time.Now().UTC().Format(time.RFC3339)
	}
	if err := tx.PutBinding(ledgerAuditHeadKind, line.Scope.String(), sequence); err != nil {
		return err
	}
	return tx.PutBinding("memory.audit/"+line.Scope.String(), fmt.Sprintf("%020d", sequence), AuditRecord{Sequence: sequence, auditLine: line})
}

func (s *LedgerStore) recordAudit(line auditLine) error {
	// Successful mutations already include their audit in the same transaction.
	if line.Err == "" && (line.Op == "remember" || line.Op == "replace" || line.Op == "forget") {
		return nil
	}
	return s.update(context.Background(), func(tx *ledger.Tx) error { return appendLedgerAudit(tx, line) })
}

func (s *LedgerStore) Audit(ctx context.Context, scope Scope) ([]AuditRecord, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	records := []AuditRecord{}
	err := s.update(ctx, func(tx *ledger.Tx) error {
		rows, err := tx.Bindings("memory.audit/" + scope.String())
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var record AuditRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence < records[j].Sequence })
	return records, err
}

func loadIdentity(tx *ledger.Tx) (identityRecord, error) {
	var identity identityRecord
	found, err := binding(tx, ledgerIdentityKind, "home", &identity)
	if err != nil {
		return identityRecord{}, err
	}
	if found && identity.Format != 1 {
		return identityRecord{}, errors.New("unsupported identity record")
	}
	identity.Format = 1
	if identity.Files == nil {
		identity.Files = map[string]string{}
	}
	for name := range identity.Files {
		if name != home.FileSoul && name != home.FileUser {
			return identityRecord{}, errors.New("shared identity contains an unsupported document")
		}
	}
	return identity, nil
}

func (s *LedgerStore) HomeBootstrapped(ctx context.Context) (bool, error) {
	var bootstrapped bool
	err := s.update(ctx, func(tx *ledger.Tx) error {
		identity, err := loadIdentity(tx)
		bootstrapped = identity.Bootstrapped
		return err
	})
	return bootstrapped, err
}

func (s *LedgerStore) BootstrapHome(ctx context.Context, files map[string]string, who Actor) (bool, error) {
	for name, text := range files {
		if budget, ok := home.Budgets[name]; !ok || len([]byte(text)) > budget {
			return false, fmt.Errorf("invalid identity document %q or exceeded budget", name)
		}
	}
	var changed bool
	err := s.update(ctx, func(tx *ledger.Tx) error {
		identity, err := loadIdentity(tx)
		if err != nil || identity.Bootstrapped {
			return err
		}
		for _, name := range []string{home.FileSoul, home.FileUser} {
			text, found := files[name]
			if !found {
				return fmt.Errorf("%w: %s", home.ErrMissing, name)
			}
			if _, exists := identity.Files[name]; !exists {
				identity.Files[name] = text
			}
		}
		if _, exists, err := loadScope(tx, Global); err != nil {
			return err
		} else if !exists {
			text, found := files[home.FileMemory]
			if !found {
				return fmt.Errorf("%w: %s", home.ErrMissing, home.FileMemory)
			}
			if err := tx.PutBinding(ledgerMemoryKind, Global.String(), scopeRecord{Format: 1, Text: text}); err != nil {
				return err
			}
		}
		identity.Bootstrapped = true
		if err := tx.PutBinding(ledgerIdentityKind, "home", identity); err != nil {
			return err
		}
		changed = true
		return appendLedgerAudit(tx, auditLine{Op: "bootstrap_home", Scope: Global, Actor: who})
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// HomeFiles reads one committed identity and global-memory view. MEMORY.md is
// rendered from the memory scope and never duplicated in the identity record.
func (s *LedgerStore) HomeFiles(ctx context.Context) (map[string]string, error) {
	files := map[string]string{}
	err := s.update(ctx, func(tx *ledger.Tx) error {
		identity, err := loadIdentity(tx)
		if err != nil {
			return err
		}
		for name, text := range identity.Files {
			files[name] = text
		}
		memory, exists, err := loadScope(tx, Global)
		if err != nil {
			return err
		}
		if exists {
			files[home.FileMemory] = stripIDs(memory.Text)
		}
		return nil
	})
	return files, err
}

func (s *LedgerStore) WriteHomeFile(ctx context.Context, name, text string, who Actor) error {
	budget, ok := home.Budgets[name]
	if !ok {
		return fmt.Errorf("unsupported identity document %q", name)
	}
	if len([]byte(text)) > budget {
		return fmt.Errorf("%s exceeds its %d byte budget", name, budget)
	}
	if name == home.FileMemory {
		return s.Replace(withActor(ctx, who), Global, text)
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		identity, err := loadIdentity(tx)
		if err != nil {
			return err
		}
		identity.Files[name] = text
		if err := tx.PutBinding(ledgerIdentityKind, "home", identity); err != nil {
			return err
		}
		return appendLedgerAudit(tx, auditLine{Op: "home_replace", Scope: Global, Actor: who, ID: name, Bytes: len([]byte(text))})
	})
}

func (s *LedgerStore) WriteIdentity(ctx context.Context, soul, user string, who Actor) error {
	soul = strings.TrimSpace(strings.ReplaceAll(soul, home.TemplateMarker, ""))
	user = strings.TrimSpace(strings.ReplaceAll(user, home.TemplateMarker, ""))
	if soul == "" || user == "" {
		return errors.New("identity is empty")
	}
	soul, user = soul+"\n", user+"\n"
	if len([]byte(soul)) > home.BudgetSoul || len([]byte(user)) > home.BudgetUser {
		return errors.New("identity exceeds its budget")
	}
	return s.update(ctx, func(tx *ledger.Tx) error {
		identity, err := loadIdentity(tx)
		if err != nil {
			return err
		}
		identity.Files[home.FileSoul], identity.Files[home.FileUser] = soul, user
		if err := tx.PutBinding(ledgerIdentityKind, "home", identity); err != nil {
			return err
		}
		return appendLedgerAudit(tx, auditLine{Op: "identity_replace", Scope: Global, Actor: who, Bytes: len([]byte(soul)) + len([]byte(user))})
	})
}
