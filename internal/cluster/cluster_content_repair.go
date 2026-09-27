package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
)

type contentRepairReport struct{ Examined, Healthy, Repaired, Degraded, Unavailable, Skipped int }
type ContentRepairObservation func(kind, subject, text string, data map[string]string)
type contentRepairWorker struct {
	peer           *Peer
	active         Activation
	client         contentreplica.Replicator
	observe        ContentRepairObservation
	previous       map[string]string
	noticeSequence uint64
}

func (p *Peer) newContentRepair(active Activation, observe ContentRepairObservation) (*contentRepairWorker, error) {
	client, err := p.ContentReplicator(active)
	if err != nil {
		return nil, err
	}
	return &contentRepairWorker{peer: p, active: active, client: client, observe: observe, previous: map[string]string{}}, nil
}

// Repair belongs to one business generation. Stopping it joins the goroutine
// before that generation's ledger and read model may be retired.
func (p *Peer) StartContentRepair(active Activation, observe ContentRepairObservation) func() {
	worker, err := p.newContentRepair(active, observe)
	if err != nil {
		slog.Error(fmt.Sprintf("content repair could not start: %v", err))
		return func() {}
	}
	ctx, cancel := context.WithCancel(active.Context)
	done := make(chan struct{})
	interval := p.Options.ContentRepairInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		defer close(done)
		for {
			if _, err := worker.sweep(ctx); err != nil && ctx.Err() == nil {
				worker.notice(ctx, "scan", "scan_failed", i18n.ClusterContentScanFailed)
			}
			worker.runMaintenance(ctx)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// notice tells the owner, in the Hub's language, what repair found for
// one object. What was said is remembered by its key and arguments, not by
// its wording, so the same finding is said once.
func (w *contentRepairWorker) notice(ctx context.Context, id, status string, key i18n.Key, args ...any) {
	if ctx.Err() != nil || w.active.Context == nil || w.active.Context.Err() != nil {
		return
	}
	w.noticeSequence++
	said := status + "\x00" + string(key)
	for _, arg := range args {
		said += "\x00" + fmt.Sprint(arg)
	}
	if w.previous[id] == said {
		return
	}
	previous := w.previous[id]
	w.previous[id] = said
	if status == "healthy" {
		if previous == "" || strings.HasPrefix(previous, "repaired\x00") || strings.HasPrefix(previous, "healthy\x00") {
			return
		}
		status, key, args = "recovered", i18n.ClusterContentRecovered, nil
	}
	message := w.peer.text.T(key, args...)
	slog.Info(fmt.Sprintf("content repair: object=%s status=%s %s", id, status, message), "object", id, "status", status)
	if w.observe != nil {
		// Keys: object.
		w.observe("content."+status, id, message, map[string]string{"object": id})
	}
}

func (w *contentRepairWorker) sweep(ctx context.Context) (contentRepairReport, error) {
	var report contentRepairReport
	if _, err := contentGenerationState(ctx, w.active); err != nil {
		return report, err
	}
	records, err := w.active.Ledger.Bindings(ctx, contentreplica.ManifestKind)
	if err != nil {
		return report, err
	}
	var ids []string
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// A peer is probed at most once per scan. Durable receipts describe the
	// bytes already acknowledged; this pass repairs node loss, not disk scrub.
	availability := map[string]bool{}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Examined++
		var manifest contentreplica.Manifest
		if err := json.Unmarshal(records[id], &manifest); err != nil || manifest.ID != id || !manifest.Complete() {
			report.Skipped++
			w.notice(ctx, id, "invalid", i18n.ClusterContentInvalid)
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		beforeNotice := w.noticeSequence
		status, err := w.repairOne(itemCtx, manifest, availability)
		cancel()
		switch status {
		case "healthy":
			report.Healthy++
		case "repaired":
			report.Repaired++
		case "unavailable":
			report.Unavailable++
		case "degraded":
			report.Degraded++
		default:
			report.Skipped++
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if generationEnded(err) {
			return report, err
		}
		if err != nil && w.noticeSequence == beforeNotice {
			key := i18n.ClusterContentItemFailed
			if errors.Is(err, context.DeadlineExceeded) {
				key = i18n.ClusterContentItemTimeout
			}
			w.notice(ctx, id, "degraded", key)
		}
	}
	return report, nil
}

// generationEnded says err is the end of the business generation a repair
// belongs to — its own or as a peer answered it. Nothing is said about the
// content: the repair stops, and the next generation checks it again.
func generationEnded(err error) bool {
	return errors.Is(err, ErrInactive) || errors.Is(err, contentreplica.ErrSuperseded) || errors.Is(err, coordination.ErrStaleEpoch) || errors.Is(err, coordination.ErrStaleWriter)
}

func (w *contentRepairWorker) reachableDomains(ctx context.Context, manifest contentreplica.Manifest, cache map[string]bool) (int, error) {
	state, err := contentGenerationState(ctx, w.active)
	if err != nil {
		return 0, err
	}
	domains := map[string]bool{}
	for _, receipt := range manifest.Receipts {
		if receipt.ObjectID != manifest.ID || receipt.StoredAt.IsZero() {
			continue
		}
		member, ok := state.Members[receipt.NodeID]
		if !ok || state.Removing[receipt.NodeID] {
			continue
		}
		where, err := (peerContentPolicy{peer: w.peer}).CheckpointPlacement(ctx, manifest.Object.Scope, receipt.NodeID)
		if errors.Is(err, contentreplica.ErrUnavailable) {
			// A copy whose placement could not be checked is not lost:
			// this round cannot count the copies.
			return 0, err
		}
		if err != nil || where.FailureDomain != receipt.FailureDomain {
			continue
		}
		if domains[where.FailureDomain] {
			continue
		}
		up, known := cache[receipt.NodeID]
		if !known {
			if receipt.NodeID == w.peer.Config.NodeID {
				up = w.peer.Runtime.Load().Status().Healthy
			} else {
				probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				progress, err := w.peer.client.Probe(probeCtx, member)
				cancel()
				up = err == nil && progress.FailureDomain == where.FailureDomain
			}
			cache[receipt.NodeID] = up
		}
		if up {
			domains[where.FailureDomain] = true
		}
	}
	return len(domains), nil
}

// uncheckedCopy is what to say about content whose copy the peers holding
// it could not check for now: their replica behind or their committed
// state out of reach. It names those peers after the content's subject.
func uncheckedCopy(text i18n.Catalog, subject []any, nodes []string) (i18n.Key, []any) {
	key := i18n.ClusterContentCopyUncheckedOne
	if len(nodes) > 1 {
		key = i18n.ClusterContentCopyUncheckedMany
	}
	return key, append(subject[:len(subject):len(subject)], strings.Join(nodes, text.T(i18n.ListSeparator)))
}

func (w *contentRepairWorker) repairOne(ctx context.Context, manifest contentreplica.Manifest, availability map[string]bool) (string, error) {
	id := manifest.ID
	// subject names the content in every notice: its project, then its ID.
	subject := []any{manifest.Object.Scope.ProjectID, id[:12]}
	scope, err := w.client.CheckLocal(ctx, manifest.Object.Scope.ProjectID)
	if generationEnded(err) {
		return "degraded", err
	}
	if errors.Is(err, contentreplica.ErrUnavailable) {
		w.notice(ctx, id, "degraded", i18n.ClusterContentPlacementUnchecked, subject...)
		return "degraded", err
	}
	if err != nil || scope != manifest.Object.Scope || scope.Level == "sealed" && scope.HomeNodeID != w.peer.Config.NodeID {
		w.notice(ctx, id, "placement_blocked", i18n.ClusterContentPlacementMismatch, subject...)
		return "placement_blocked", errors.Join(contentreplica.ErrPlacement, err)
	}
	state, err := contentGenerationState(ctx, w.active)
	if err != nil {
		return "degraded", err
	}
	required := manifest.RequiredCopies
	if scope.Level != "sealed" && len(state.Members) > 1 {
		required = 2
	}
	live, err := w.reachableDomains(ctx, manifest, availability)
	if err != nil {
		return "degraded", err
	}
	if live >= required {
		if manifest.RequiredCopies < required {
			manifest.RequiredCopies = required
			manifest.Protection = contentreplica.Replicated
			if err := w.record(ctx, manifest); err != nil {
				return "degraded", err
			}
			w.notice(ctx, id, "repaired", i18n.ClusterContentReplicated, subject...)
			return "repaired", nil
		}
		w.notice(ctx, id, "healthy", "")
		return "healthy", nil
	}
	if err := os.MkdirAll(filepath.Join(w.peer.Config.DataDir, "content-repair"), 0o700); err != nil {
		return "degraded", err
	}
	file, err := os.CreateTemp(filepath.Join(w.peer.Config.DataDir, "content-repair"), "repair-*")
	if err != nil {
		return "degraded", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	available, err := w.client.Read(ctx, manifest, file)
	if err != nil {
		return w.readFailed(ctx, id, subject, err)
	}
	availability[w.peer.Config.NodeID] = true
	// Persist this exact local upload before a separate preparation can
	// supersede its receipt. Otherwise it would remain an unknown promise.
	if err := w.record(ctx, available); err != nil {
		return "degraded", err
	}
	live, err = w.reachableDomains(ctx, available, availability)
	if err != nil {
		return "degraded", err
	}
	if live < required {
		if _, err := file.Seek(0, 0); err != nil {
			return "degraded", err
		}
		prepared, prepareErr := w.prepareCopy(ctx, scope, manifest, file)
		if prepareErr != nil {
			return w.prepareFailed(ctx, id, subject, available, prepareErr, live, required)
		}
		available = prepared
		for _, receipt := range prepared.Receipts {
			availability[receipt.NodeID] = true
		}
		live, err = w.reachableDomains(ctx, available, availability)
		if err != nil {
			return "degraded", err
		}
	}
	if live < required {
		w.notice(ctx, id, "degraded", i18n.ClusterContentShort, subject...)
		return "degraded", contentreplica.ErrIncomplete
	}
	if required == 2 {
		available.RequiredCopies = 2
		available.Protection = contentreplica.Replicated
	}
	if err := w.record(ctx, available); err != nil {
		w.notice(ctx, id, "degraded", i18n.ClusterContentUnconfirmed, subject...)
		return "degraded", err
	}
	w.notice(ctx, id, "repaired", i18n.ClusterContentRepaired, subject...)
	return "repaired", nil
}

// readFailed is how a repair ends when it could not read a verified copy
// into this node: the status it reports and the notice it gives, by what
// stopped the read. A generation that ended or a repair that ran out of
// time says nothing about the content.
func (w *contentRepairWorker) readFailed(ctx context.Context, id string, subject []any, err error) (string, error) {
	switch {
	case generationEnded(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "degraded", err
	case errors.Is(err, checkpoint.ErrQuota):
		w.notice(ctx, id, "degraded", i18n.ClusterContentLocalQuota, subject...)
		return "degraded", err
	case errors.Is(err, errContentPeerUnchecked):
		key, args := uncheckedCopy(w.peer.text, subject, uncheckedContentPeers(err))
		w.notice(ctx, id, "degraded", key, args...)
		return "degraded", err
	case errors.Is(err, contentreplica.ErrUnavailable):
		w.notice(ctx, id, "degraded", i18n.ClusterContentPlacementUnchecked, subject...)
		return "degraded", err
	case errors.Is(err, contentreplica.ErrPlacement):
		w.notice(ctx, id, "placement_blocked", i18n.ClusterContentPlacementChanged, subject...)
		return "placement_blocked", err
	case errors.Is(err, contentreplica.ErrIncomplete):
		w.notice(ctx, id, "unavailable", i18n.ClusterContentNoVerifiedCopy, subject...)
		return "unavailable", err
	default:
		w.notice(ctx, id, "degraded", i18n.ClusterContentReadIncomplete, subject...)
		return "degraded", err
	}
}

// prepareCopy stores the copy staged in file on further nodes, as the kind
// of content it is.
func (w *contentRepairWorker) prepareCopy(ctx context.Context, scope contentreplica.Scope, manifest contentreplica.Manifest, file io.ReadSeeker) (contentreplica.Manifest, error) {
	if manifest.Object.Kind == contentreplica.GitBundle {
		return w.client.PrepareBundle(ctx, scope.ProjectID, manifest.Object.Key, manifest.Object.Base, manifest.Object.Blob, file)
	}
	return w.client.Prepare(ctx, scope.ProjectID, manifest.Object.Kind, manifest.Object.Key, manifest.Object.Blob, file)
}

// prepareFailed is how a repair ends when it recovered this node's copy but
// could not store another: the recovered receipt is still recorded, and the
// notice says what is short. A generation that ended says nothing.
func (w *contentRepairWorker) prepareFailed(ctx context.Context, id string, subject []any, available contentreplica.Manifest, prepareErr error, live, required int) (string, error) {
	if generationEnded(prepareErr) {
		return "degraded", prepareErr
	}
	// Persist a newly recovered local receipt even when no second target
	// is currently available; Record never reduces existing protection.
	recordErr := w.record(ctx, available)
	key, args := i18n.ClusterContentFewCopies, append(subject[:len(subject):len(subject)], live, required)
	if errors.Is(prepareErr, checkpoint.ErrQuota) {
		key, args = i18n.ClusterContentRemoteQuota, subject
	}
	if recordErr != nil {
		key, args = i18n.ClusterContentRecordPending, subject
	}
	w.notice(ctx, id, "degraded", key, args...)
	return "degraded", errors.Join(prepareErr, recordErr)
}

func (w *contentRepairWorker) record(ctx context.Context, manifest contentreplica.Manifest) error {
	if _, err := contentGenerationState(ctx, w.active); err != nil {
		return err
	}
	return w.active.Ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, manifest); return err })
}
