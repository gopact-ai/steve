package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
)

type contentRepairReport struct{ Examined, Healthy, Repaired, Degraded, Unavailable, Skipped int }
type ContentRepairObservation func(kind, subject, text string)
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
		log.Printf("content repair could not start: %v", err)
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
				worker.notice(ctx, "scan", "scan_failed", "协作内容副本检查暂时无法完成；恢复协调连接后会继续检查。")
			}
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

func (w *contentRepairWorker) notice(ctx context.Context, id, status, message string) {
	if ctx.Err() != nil || w.active.Context == nil || w.active.Context.Err() != nil {
		return
	}
	w.noticeSequence++
	key := status + "\x00" + message
	if w.previous[id] == key {
		return
	}
	previous := w.previous[id]
	w.previous[id] = key
	if status == "healthy" {
		if previous == "" || strings.HasPrefix(previous, "repaired\x00") || strings.HasPrefix(previous, "healthy\x00") {
			return
		}
		status = "recovered"
		message = "此前等待的协作内容副本已恢复可用。"
	}
	log.Printf("content repair: object=%s status=%s %s", id, status, message)
	if w.observe != nil {
		w.observe("content."+status, id, message)
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
			w.notice(ctx, id, "invalid", "协作内容的副本记录无法验证，需要恢复有效记录后再继续复制。")
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
		if err != nil && (errors.Is(err, ErrInactive) || errors.Is(err, coordination.ErrStaleEpoch) || errors.Is(err, coordination.ErrStaleWriter)) {
			return report, err
		}
		if err != nil && w.noticeSequence == beforeNotice {
			message := "此项协作内容的补副本未完成，请检查本机存储与节点连接；其他独立内容继续处理。"
			if errors.Is(err, context.DeadlineExceeded) {
				message = "此项协作内容的补副本超过本次等待时间，其他内容继续处理，下一轮将重新核对。"
			}
			w.notice(ctx, id, "degraded", message)
		}
	}
	return report, nil
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

func (w *contentRepairWorker) repairOne(ctx context.Context, manifest contentreplica.Manifest, availability map[string]bool) (string, error) {
	id := manifest.ID
	label := fmt.Sprintf("项目 %s 的内容 %s", manifest.Object.Scope.ProjectID, id[:12])
	scope, err := w.client.CheckLocal(ctx, manifest.Object.Scope.ProjectID)
	if err != nil || scope != manifest.Object.Scope || scope.Level == "sealed" && scope.HomeNodeID != w.peer.Config.NodeID {
		w.notice(ctx, id, "placement_blocked", label+" 的当前存储授权与原记录不一致，已停止补副本。")
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
			w.notice(ctx, id, "repaired", label+" 已具备两个独立节点的副本。")
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
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return "degraded", err
		case errors.Is(err, checkpoint.ErrQuota):
			w.notice(ctx, id, "degraded", label+" 暂时无法在本机保存副本，请检查存储配额；已有副本记录保持不变。")
			return "degraded", err
		case errors.Is(err, contentreplica.ErrPlacement):
			w.notice(ctx, id, "placement_blocked", label+" 的存储授权已改变，等待确认可用存储位置后再复制。")
			return "placement_blocked", err
		case errors.Is(err, contentreplica.ErrIncomplete):
			w.notice(ctx, id, "unavailable", label+" 当前无法取得已验证副本，请检查原节点连接与内容状态；恢复后会重新核对。")
			return "unavailable", err
		default:
			w.notice(ctx, id, "degraded", label+" 的读取或本机保存尚未完成，已有副本记录保持不变，请检查存储和连接。")
			return "degraded", err
		}
	}
	availability[w.peer.Config.NodeID] = true
	live, err = w.reachableDomains(ctx, available, availability)
	if err != nil {
		return "degraded", err
	}
	if live < required {
		if _, err := file.Seek(0, 0); err != nil {
			return "degraded", err
		}
		prepared, prepareErr := w.client.Prepare(ctx, scope.ProjectID, manifest.Object.Kind, manifest.Object.Key, manifest.Object.Blob, file)
		if prepareErr != nil {
			// Persist a newly recovered local receipt even when no second target
			// is currently available; Record never reduces existing protection.
			recordErr := w.record(ctx, available)
			message := fmt.Sprintf("%s 当前只有 %d 个独立可达副本，需要 %d 个；等待符合存储授权的节点恢复后继续补齐。", label, live, required)
			if errors.Is(prepareErr, checkpoint.ErrQuota) {
				message = label + " 暂时无法增加独立副本，请检查可用节点的存储配额。"
			}
			if recordErr != nil {
				message = label + " 的本机内容已恢复，但副本记录尚未提交，后续会重新核对。"
			}
			w.notice(ctx, id, "degraded", message)
			return "degraded", errors.Join(prepareErr, recordErr)
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
		w.notice(ctx, id, "degraded", label+" 尚未恢复所需的独立副本，原保护要求保持不变。")
		return "degraded", contentreplica.ErrIncomplete
	}
	if required == 2 {
		available.RequiredCopies = 2
		available.Protection = contentreplica.Replicated
	}
	if err := w.record(ctx, available); err != nil {
		w.notice(ctx, id, "degraded", label+" 的新副本已保存，但协作账本尚未确认；后续会重新核对。")
		return "degraded", err
	}
	w.notice(ctx, id, "repaired", label+" 已在可用节点补齐独立副本。")
	return "repaired", nil
}

func (w *contentRepairWorker) record(ctx context.Context, manifest contentreplica.Manifest) error {
	if _, err := contentGenerationState(ctx, w.active); err != nil {
		return err
	}
	return w.active.Ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := contentreplica.Record(tx, manifest); return err })
}
