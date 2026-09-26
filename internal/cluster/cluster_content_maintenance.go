package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/contentreplica"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Maintenance sends no deletion candidates. Each receiver independently
// validates its committed owner roots and exact upload-release proofs.
func (transport peerContentTransport) collect(ctx context.Context, node string) (checkpoint.RetentionGCResult, error) {
	state, err := contentGenerationState(ctx, transport.active)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	member, found := state.Members[node]
	if !found || state.Removing[node] || state.Coordinator.NodeID != transport.peer.Config.NodeID {
		return checkpoint.RetentionGCResult{}, contentreplica.ErrPlacement
	}
	clientTransport, origin, err := transport.peer.remoteTransport(member)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.String()+clusterContentPath, nil)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	request.Header.Set("X-Steve-Coordinator-Epoch", strconv.FormatUint(state.Coordinator.Epoch, 10))
	request.Header.Set("X-Steve-Writer-Generation", strconv.FormatUint(state.WriterGeneration, 10))
	client := &http.Client{Transport: clientTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return checkpoint.RetentionGCResult{}, fmt.Errorf("content maintenance: %w", contentReplyError(node, response))
	}
	var result checkpoint.RetentionGCResult
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32769))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || len(result.Released) > 256 || result.Blobs < 0 || result.Bytes < 0 {
		return checkpoint.RetentionGCResult{}, contentreplica.ErrIntegrity
	}
	return result, nil
}

func (p *Peer) collectContent(ctx context.Context) (checkpoint.RetentionGCResult, error) {
	runtime := p.Runtime.Load()
	if runtime == nil {
		return checkpoint.RetentionGCResult{}, ErrInactive
	}
	state, err := p.committedState(ctx, runtime)
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	if _, err := p.awaitContentReplica(ctx, runtime, state.AppVersion); err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	store, release, err := p.acquireContent()
	if err != nil {
		return checkpoint.RetentionGCResult{}, err
	}
	defer release()
	return store.GC(ctx, runtime.Ledger())
}

func (p *Peer) serveContentMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 || r.Header.Get(contentObjectHeader) != "" || r.Header.Get(contentUploadHeader) != "" {
		writeContentReply(w, http.StatusBadRequest, "invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := p.collectContent(ctx)
	if err != nil {
		writeContentError(w, err)
		return
	}
	// Collecting may have taken a while: the caller's authority is read
	// again, not taken from the read that admitted it.
	if err := p.contentAuthority(r.WithContext(withContentReads(r.Context()))); err != nil {
		writeContentError(w, err)
		return
	}
	WriteJSON(w, result)
}

func (w *contentRepairWorker) maintain(ctx context.Context) (checkpoint.GCResult, error) {
	state, err := contentGenerationState(ctx, w.active)
	if err != nil {
		return checkpoint.GCResult{}, err
	}
	if err := w.active.Ledger.Update(ctx, contentreplica.ReleaseSuperseded); err != nil {
		return checkpoint.GCResult{}, err
	}
	nodes := make([]string, 0, len(state.Members))
	for node := range state.Members {
		if !state.Removing[node] {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	var total checkpoint.GCResult
	var failures []error
	transport := peerContentTransport{peer: w.peer, active: w.active}
	for _, node := range nodes {
		if _, err := contentGenerationState(ctx, w.active); err != nil {
			return total, errors.Join(append(failures, err)...)
		}
		itemCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var result checkpoint.RetentionGCResult
		if node == w.peer.Config.NodeID {
			result, err = w.peer.collectContent(itemCtx)
		} else {
			result, err = transport.collect(itemCtx, node)
		}
		if err == nil && len(result.Released) != 0 {
			err = w.active.Ledger.Update(itemCtx, func(tx *ledger.Tx) error {
				return contentreplica.ConfirmCollection(tx, node, result.Released)
			})
		}
		cancel()
		total.Blobs += result.Blobs
		total.Bytes += result.Bytes
		if err != nil {
			failures = append(failures, fmt.Errorf("content maintenance %s: %w", node, err))
		}
	}
	return total, errors.Join(failures...)
}

func (w *contentRepairWorker) runMaintenance(ctx context.Context) {
	result, err := w.maintain(ctx)
	if err != nil {
		w.notice(ctx, "maintenance", "gc_failed", fmt.Sprintf("内容回执清理未完成，未知上传仍保留：%v", err))
	} else if result.Blobs > 0 {
		w.notice(ctx, "maintenance", "gc_completed", fmt.Sprintf("已按持久释放凭据清理 %d 个内容对象（%d 字节），未知上传保持不变。", result.Blobs, result.Bytes))
	} else {
		delete(w.previous, "maintenance")
	}
}
