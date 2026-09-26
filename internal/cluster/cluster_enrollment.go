package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/fsx"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/sameorigin"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type PeerEnrollmentRequest struct {
	Alias       string `json:"alias"`
	Name        string `json:"name"`
	PeerAddress string `json:"peer_address"`
	RaftAddress string `json:"raft_address"`
	Level       string `json:"level,omitempty"`
	// HubRoute is where the machine reaches this node: the loopback ports
	// on the machine at which this node's Raft and API listeners appear
	// through the SSH session the enrollment opens. Set for every machine
	// enrolled over SSH; the machine never has to route to this node.
	HubRoute coordination.Route `json:"hub_route"`
	// WorkspaceDir is where the machine keeps its work: its default project
	// directory and the root its executor runs in. "~/" is the remote home.
	WorkspaceDir     string `json:"workspace_dir,omitempty"`
	ExpectedPlanHash string `json:"expected_plan_hash,omitempty"`
}

type PeerEnrollmentPlan struct {
	Request   PeerEnrollmentRequest `json:"request"`
	ClusterID string                `json:"cluster_id"`
	Seeds     []coordination.Member `json:"seeds"`
	Effects   []string              `json:"effects"`
	ReviewID  string                `json:"review_id"`
}

// PeerEnrollmentPackage is kept between the owner service and SSH stdin. It is
// deliberately not serialized into a UI installation plan.
type PeerEnrollmentPackage struct {
	NodeID  string             `json:"-"`
	Payload []byte             `json:"-"`
	Plan    PeerEnrollmentPlan `json:"-"`
}

type PeerJoinPackage struct {
	Version       int                   `json:"version"`
	OperationID   string                `json:"operation_id"`
	ClusterID     string                `json:"cluster_id"`
	NodeID        string                `json:"node_id"`
	Name          string                `json:"name"`
	StorageLevel  string                `json:"storage_level"`
	WorkspaceDir  string                `json:"workspace_dir,omitempty"`
	PeerListen    string                `json:"peer_listen"`
	PeerAdvertise string                `json:"peer_advertise"`
	RaftListen    string                `json:"raft_listen"`
	RaftAdvertise string                `json:"raft_advertise"`
	CA            []byte                `json:"ca"`
	Certificate   []byte                `json:"certificate"`
	PrivateKey    []byte                `json:"private_key"`
	OwnerToken    string                `json:"owner_token"`
	WorkerToken   string                `json:"worker_token"`
	Seeds         []coordination.Member `json:"seeds"`
	// Routes is where the machine connects to members it cannot reach at
	// the addresses they advertise: this node, through the SSH session.
	Routes map[string]coordination.Route `json:"routes,omitempty"`
	// Locale is the enrolling Hub's language: the machine says what it
	// refuses in it while importing, and speaks it from then on.
	Locale i18n.Locale `json:"locale,omitempty"`
}

type PeerEnrollmentStep struct {
	At      time.Time `json:"at"`
	Stage   string    `json:"stage"`
	Message string    `json:"message"`
}

type PeerEnrollmentResult struct {
	OperationID string               `json:"operation_id"`
	NodeID      string               `json:"node_id"`
	Name        string               `json:"name"`
	Phase       string               `json:"phase"`
	Ready       bool                 `json:"ready"`
	Steps       []PeerEnrollmentStep `json:"steps"`
	Error       string               `json:"error,omitempty"`
}

type peerEnrollmentRecord struct {
	PeerEnrollmentResult
	Request     PeerEnrollmentRequest `json:"request"`
	Fingerprint string                `json:"fingerprint"`
	Package     []byte                `json:"package"`
	Plan        PeerEnrollmentPlan    `json:"plan"`
}

type advertisedPeerListener struct {
	net.Listener
	address *atomic.Value
}

type peerNetworkAddress string

func (a peerNetworkAddress) Network() string { return "tcp" }

func (a peerNetworkAddress) String() string { return string(a) }

func (l *advertisedPeerListener) Addr() net.Addr {
	if address, ok := l.address.Load().(string); ok {
		return peerNetworkAddress(address)
	}
	return l.Listener.Addr()
}

func boundAdvertiseAddress(configured, bound string) string {
	host, port, err := net.SplitHostPort(configured)
	if err != nil {
		return bound
	}
	if port == "0" {
		_, boundPort, err := net.SplitHostPort(bound)
		if err != nil {
			return bound
		}
		port = boundPort
	}
	return net.JoinHostPort(host, port)
}

// localAdvertiseAddresses lists this machine's IPv4 addresses, LAN
// interfaces first and tunnels (VPN) after them: a tunnel is often the
// only path a remote machine has back to a laptop.
func localAdvertiseAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var lan, tunnels []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		entries, _ := iface.Addrs()
		for _, entry := range entries {
			ip, _, err := net.ParseCIDR(entry.String())
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.To4() == nil {
				continue
			}
			if iface.Flags&net.FlagPointToPoint != 0 {
				tunnels = append(tunnels, ip.String())
			} else {
				lan = append(lan, ip.String())
			}
		}
	}
	for _, group := range [][]string{lan, tunnels} {
		sort.Slice(group, func(i, j int) bool {
			a, b := net.ParseIP(group[i]), net.ParseIP(group[j])
			if a.IsPrivate() != b.IsPrivate() {
				return a.IsPrivate()
			}
			return group[i] < group[j]
		})
	}
	return append(lan, tunnels...)
}

func validPeerEndpoint(text i18n.Catalog, address string, allowLoopback bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New(text.T(i18n.ClusterAddressNeedsHostPort))
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || host == "" {
		return errors.New(text.T(i18n.ClusterPortRange))
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || !allowLoopback && ip.IsLoopback() {
			return errors.New(text.T(i18n.ClusterAddressNotLAN))
		}
	} else if strings.ContainsAny(host, "/\\\x00\r\n\t ") || !allowLoopback && strings.EqualFold(host, "localhost") {
		return errors.New(text.T(i18n.ClusterHostInvalid))
	}
	return nil
}

func (p *Peer) PreviewPeerEnrollment(ctx context.Context, request PeerEnrollmentRequest) (PeerEnrollmentPlan, error) {
	return p.PreviewEnrollment(ctx, request, false)
}

func (p *Peer) PreviewEnrollment(ctx context.Context, request PeerEnrollmentRequest, allowLoopback bool) (PeerEnrollmentPlan, error) {
	text := p.text.For(ctx)
	request.ExpectedPlanHash = ""
	name, err := coordination.MemberName(request.Name)
	if err != nil {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterMachineNameInvalid))
	}
	request.Name = name
	if request.WorkspaceDir = strings.TrimSpace(request.WorkspaceDir); request.WorkspaceDir == "" {
		request.WorkspaceDir = DefaultPeerWorkspace
	}
	if err := validPeerWorkspace(text, request.WorkspaceDir); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if err := validPeerEndpoint(text, request.PeerAddress, allowLoopback); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	host, port, _ := net.SplitHostPort(request.PeerAddress)
	if request.RaftAddress == "" {
		n, _ := strconv.Atoi(port)
		if n >= 65535 {
			return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterRaftPortNeeded))
		}
		request.RaftAddress = net.JoinHostPort(host, strconv.Itoa(n+1))
	}
	if err := validPeerEndpoint(text, request.RaftAddress, allowLoopback); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if request.PeerAddress == request.RaftAddress {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterPortsSame))
	}
	routed, err := validHubRoute(text, request.HubRoute)
	if err != nil {
		return PeerEnrollmentPlan{}, err
	}
	level := datalevel.Level(request.Level).OrDefault()
	if level != datalevel.Public && level != datalevel.Internal && level != datalevel.Restricted && level != datalevel.Sealed {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterLevelInvalid))
	}
	request.Level = string(level)
	if request.Level != "restricted" && request.Level != "sealed" {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterLevelNeedsLedger))
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return PeerEnrollmentPlan{}, coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if err := p.authorizeLedgerReplica(ctx, coordination.Member{StorageLevel: request.Level}); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if _, ok := state.Members[p.Config.NodeID]; !ok {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterNotJoined))
	}
	if p.Config.CAKeyFile == "" {
		return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterNeedsSigningApp))
	}
	plan := PeerEnrollmentPlan{Request: request, ClusterID: state.ClusterID}
	for id, member := range state.Members {
		if member.Address == request.RaftAddress || member.APIAddress == "https://"+request.PeerAddress {
			return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterPortTaken))
		}
		if state.Voters[id] == "" {
			continue
		}
		// The machine reaches this node through the session, whatever this
		// node advertises; the other members it must reach on its own.
		if id != p.Config.NodeID || !routed {
			if err := validPeerEndpoint(text, member.Address, allowLoopback); err != nil {
				return PeerEnrollmentPlan{}, fmt.Errorf(text.T(i18n.ClusterMemberUnreachable), member.NodeID, err)
			}
			endpoint, err := url.Parse(member.APIAddress)
			if err != nil || endpoint.Scheme != "https" {
				return PeerEnrollmentPlan{}, errors.New(text.T(i18n.ClusterMemberNoHTTPS))
			}
			if err := validPeerEndpoint(text, endpoint.Host, allowLoopback); err != nil {
				return PeerEnrollmentPlan{}, err
			}
		}
		plan.Seeds = append(plan.Seeds, member)
	}
	sort.Slice(plan.Seeds, func(i, j int) bool { return plan.Seeds[i].NodeID < plan.Seeds[j].NodeID })
	plan.Effects = plan.effects(text)
	plan.ReviewID = plan.reviewHash()
	return plan, nil
}

// effects says what the plan will do, in text's language. It reads nothing
// but the plan, all of which its review ID covers, so the sentences can be
// left out of that ID without leaving any effect unreviewed.
func (plan PeerEnrollmentPlan) effects(text i18n.Catalog) []string {
	request := plan.Request
	var effects []string
	if request.HubRoute != (coordination.Route{}) {
		effects = append(effects, text.T(i18n.ClusterEffectTunnel, request.HubRoute.Raft, request.HubRoute.API))
	}
	return append(effects, text.T(i18n.ClusterEffectStart, request.PeerAddress, request.RaftAddress), text.T(i18n.ClusterEffectWorkspace, request.WorkspaceDir), text.T(i18n.ClusterEffectIdentity), text.T(i18n.ClusterEffectLedger), text.T(i18n.ClusterEffectVoting), text.T(i18n.ClusterEffectSuccess))
}

// reviewHash identifies what the owner reviewed: every fact of the plan.
// The effects are left out: they are those facts said in the reviewer's
// language, so the same plan read in two languages is still one plan.
func (plan PeerEnrollmentPlan) reviewHash() string {
	plan.ReviewID = ""
	plan.Effects = nil
	plan.Request.ExpectedPlanHash = ""
	data, _ := json.Marshal(plan)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (p *Peer) PreparePeerEnrollment(ctx context.Context, request PeerEnrollmentRequest, id string) (PeerEnrollmentPackage, error) {
	return p.PrepareEnrollment(ctx, request, id, false)
}

func (p *Peer) PrepareEnrollment(ctx context.Context, request PeerEnrollmentRequest, id string, allowLoopback bool) (PeerEnrollmentPackage, error) {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	text := p.text.For(ctx)
	if strings.TrimSpace(id) == "" {
		return PeerEnrollmentPackage{}, errors.New(text.T(i18n.ClusterOperationIDEmpty))
	}
	encoded, _ := json.Marshal(request)
	hash := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(hash[:])
	if existing, err := p.loadEnrollment(id); err == nil {
		if existing.Fingerprint != fingerprint {
			return PeerEnrollmentPackage{}, coordination.ErrCommandConflict
		}
		return PeerEnrollmentPackage{NodeID: existing.NodeID, Payload: existing.Package, Plan: existing.Plan}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PeerEnrollmentPackage{}, err
	}
	plan, err := p.PreviewEnrollment(ctx, request, allowLoopback)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	if request.ExpectedPlanHash == "" || request.ExpectedPlanHash != plan.ReviewID {
		return PeerEnrollmentPackage{}, fmt.Errorf(text.T(i18n.ClusterPlanChangedReview), coordination.ErrConflict)
	}
	caPEM, err := ReadClusterPrivate(p.Config.CACertFile)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return PeerEnrollmentPackage{}, errors.New(text.T(i18n.ClusterCACertUnreadable))
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	keyPEM, err := ReadClusterPrivate(p.Config.CAKeyFile)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return PeerEnrollmentPackage{}, errors.New(text.T(i18n.ClusterCAKeyUnreadable))
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return PeerEnrollmentPackage{}, errors.New(text.T(i18n.ClusterCAKeyUnsupported))
	}
	nodeID := clusterRandomID("node-")
	certificate, leafKey, err := IssueNodeCertificate(ca, privateKey, p.Config.ClusterID, nodeID)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	workerToken := ClusterRandomToken()
	_, peerPort, _ := net.SplitHostPort(plan.Request.PeerAddress)
	_, raftPort, _ := net.SplitHostPort(plan.Request.RaftAddress)
	bundle := PeerJoinPackage{Version: 1, OperationID: id, ClusterID: p.Config.ClusterID, NodeID: nodeID, StorageLevel: plan.Request.Level, Name: plan.Request.Name, WorkspaceDir: plan.Request.WorkspaceDir, PeerListen: net.JoinHostPort("0.0.0.0", peerPort), PeerAdvertise: plan.Request.PeerAddress, RaftListen: net.JoinHostPort("0.0.0.0", raftPort), RaftAdvertise: plan.Request.RaftAddress, Locale: p.text.Locale(), CA: caPEM, Certificate: certificate, PrivateKey: leafKey, OwnerToken: p.OwnerToken, WorkerToken: workerToken, Seeds: plan.Seeds}
	if plan.Request.HubRoute.Raft != "" {
		bundle.Routes = map[string]coordination.Route{p.Config.NodeID: plan.Request.HubRoute}
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	record := peerEnrollmentRecord{PeerEnrollmentResult: PeerEnrollmentResult{OperationID: id, NodeID: nodeID, Name: plan.Request.Name, Phase: "prepared", Steps: []PeerEnrollmentStep{{At: time.Now().UTC(), Stage: "prepared", Message: text.T(i18n.ClusterEnrollmentPrepared)}}}, Request: plan.Request, Fingerprint: fingerprint, Package: payload, Plan: plan}
	if err := p.saveEnrollment(record); err != nil {
		return PeerEnrollmentPackage{}, err
	}
	return PeerEnrollmentPackage{NodeID: nodeID, Payload: payload, Plan: plan}, nil
}

// validHubRoute accepts a route that is either absent or a pair of
// loopback host:port addresses on the machine. It answers whether the
// enrollment is routed.
func validHubRoute(text i18n.Catalog, route coordination.Route) (bool, error) {
	if route.Raft == "" && route.API == "" {
		return false, nil
	}
	if err := loopbackRoute(text, route); err != nil {
		return false, err
	}
	return true, nil
}

// loopbackRoute accepts two distinct loopback host:port addresses with
// real ports: a tunnel's ends are fixed ports, never 0.
func loopbackRoute(text i18n.Catalog, route coordination.Route) error {
	for _, address := range []string{route.Raft, route.API} {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return errors.New(text.T(i18n.ClusterTunnelHostPort))
		}
		if !sameorigin.LoopbackIP(address) {
			return errors.New(text.T(i18n.ClusterTunnelLoopback))
		}
		if number, err := strconv.Atoi(port); err != nil || number <= 0 || number > 65535 {
			return errors.New(text.T(i18n.ClusterTunnelFixedPort))
		}
	}
	if route.Raft == route.API {
		return errors.New(text.T(i18n.ClusterTunnelPortsDiffer))
	}
	return nil
}

func (p *Peer) enrollmentPath(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(p.Config.DataDir, "enrollments", hex.EncodeToString(digest[:])+".json")
}

func (p *Peer) loadEnrollment(id string) (peerEnrollmentRecord, error) {
	var record peerEnrollmentRecord
	data, err := ReadClusterPrivate(p.enrollmentPath(id))
	if err != nil {
		return record, err
	}
	err = json.Unmarshal(data, &record)
	if err == nil && record.OperationID != id {
		err = coordination.ErrCommandConflict
	}
	return record, err
}

func (p *Peer) saveEnrollment(record peerEnrollmentRecord) error {
	if err := os.MkdirAll(filepath.Dir(p.enrollmentPath(record.OperationID)), 0o700); err != nil {
		return err
	}
	return SaveClusterJSON(p.enrollmentPath(record.OperationID), record, false)
}

// peerCatchUpSlice bounds one CompletePeerEnrollment call while the new
// node copies the ledger; the caller polls again for the next slice.
const peerCatchUpSlice = 3 * time.Second

func (p *Peer) CompletePeerEnrollment(ctx context.Context, id string) (PeerEnrollmentResult, error) {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	// The record keeps what happened in the language of whoever is
	// waiting on it, or the Hub's when another node asks.
	text := p.text.For(ctx)
	record, err := p.loadEnrollment(id)
	if err != nil {
		return PeerEnrollmentResult{}, err
	}
	if record.Ready {
		return record.PeerEnrollmentResult, nil
	}
	finish := func(stage string, err error) (PeerEnrollmentResult, error) {
		record.Phase = stage
		if err != nil {
			record.Error = err.Error()
		} else {
			record.Error = ""
		}
		// A poll that finds the same thing as the last one adds nothing to
		// the record; the record is polled for as long as the wait lasts.
		if n := len(record.Steps); n == 0 || record.Steps[n-1].Stage != stage || record.Steps[n-1].Message != record.Error {
			record.Steps = append(record.Steps, PeerEnrollmentStep{At: time.Now().UTC(), Stage: stage, Message: record.Error})
		}
		saveErr := p.saveEnrollment(record)
		return record.PeerEnrollmentResult, errors.Join(err, saveErr)
	}
	member := coordination.Member{NodeID: record.NodeID, Name: record.Name, Address: record.Request.RaftAddress, APIAddress: "https://" + record.Request.PeerAddress, AutoEligible: false, StorageLevel: record.Request.Level, Voting: false}
	status, err := p.client.Status(ctx, member)
	if err != nil {
		return finish("awaiting_peer", fmt.Errorf(text.T(i18n.ClusterAwaitingPeer), err))
	}
	if !status.Healthy {
		return finish("awaiting_peer", coordination.ErrNotReady)
	}
	_, err = p.Runtime.Load().Join(ctx, coordination.JoinRequest{ID: id + "/join", Actor: "owner", Member: member})
	if err != nil {
		return finish("synchronizing", err)
	}
	if _, err := finish("joined", nil); err != nil {
		return record.PeerEnrollmentResult, err
	}
	state, err := p.Runtime.Load().ReadState(ctx)
	if err != nil {
		return finish("registering_worker", err)
	}
	coordinator, ok := state.Members[state.Coordinator.NodeID]
	if !ok {
		return finish("registering_worker", coordination.ErrUnavailable)
	}
	var registered struct {
		OK bool `json:"ok"`
	}
	err = p.peerJSON(ctx, coordinator, http.MethodPost, "/cluster/enrollment/register-worker", map[string]string{"operation_id": id, "node_id": record.NodeID, "level": record.Request.Level}, &registered)
	if err != nil || !registered.OK {
		if err == nil {
			err = errors.New(text.T(i18n.ClusterWorkerUnconfirmed))
		}
		return finish("registering_worker", err)
	}
	state, err = p.Runtime.Load().ReadState(ctx)
	if err != nil {
		return finish("synchronizing", err)
	}
	// The catch-up is reported in slices: each call returns within a few
	// seconds with how far the replica has come, so a caller polling the
	// operation sees the copy advance instead of one silent long call.
	deadline := time.Now().Add(peerCatchUpSlice)
	for {
		progress, err := p.client.Probe(ctx, member)
		if err == nil && progress.AppVersion >= state.AppVersion && progress.AppliedIndex >= state.AppliedIndex {
			break
		}
		if time.Now().After(deadline) {
			return finish("synchronizing", fmt.Errorf(text.T(i18n.ClusterReplicating), coordination.ErrNotReady, progress.AppliedIndex, state.AppliedIndex))
		}
		select {
		case <-ctx.Done():
			return finish("synchronizing", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	record.Ready = true
	return finish("ready", nil)
}

func (p *Peer) PeerEnrollmentStatus(_ context.Context, id string) (PeerEnrollmentResult, error) {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	record, err := p.loadEnrollment(id)
	return record.PeerEnrollmentResult, err
}

// AbandonPeerEnrollment gives up an enrollment that did not finish. The
// member a failed join left in the cluster is removed, so the machine's
// ports are free to plan again, and the record is set aside rather than
// deleted. A node that did join is a member now and leaves from the
// resources page, where removing it is its own reviewed action.
func (p *Peer) AbandonPeerEnrollment(ctx context.Context, id string) error {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	text := p.text.For(ctx)
	record, err := p.loadEnrollment(id)
	if errors.Is(err, os.ErrNotExist) {
		return saidError{text.T(i18n.ClusterEnrollmentGone), ErrEnrollmentGone}
	}
	if err != nil {
		return err
	}
	if record.Ready {
		return saidError{text.T(i18n.ClusterEnrollmentJoined), ErrEnrollmentJoined}
	}
	if err := p.leaveCluster(ctx, id+"/abandon", record.NodeID); err != nil {
		return fmt.Errorf(text.T(i18n.ClusterAbandonWithdrawFailed), err)
	}
	path := p.enrollmentPath(id)
	archived := path + ".abandoned-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, archived); err != nil {
		return fmt.Errorf(text.T(i18n.ClusterAbandonArchiveFailed), err)
	}
	return nil
}

// DefaultPeerWorkspace is where a machine keeps its work unless the owner
// chooses somewhere: a visible directory under the remote account's home.
const DefaultPeerWorkspace = sshconnect.DefaultWorkspaceDir

// ErrEnrollmentGone says there is no such enrollment to give up; a caller
// that only knows the operation ID has nothing left to do. It is matched
// with errors.Is; the reader is told in their language.
var ErrEnrollmentGone = errors.New("enrollment record is gone")

// ErrEnrollmentJoined says the machine is a member now: leaving the cluster
// is its own reviewed action on the resources page, not an enrollment undo.
var ErrEnrollmentJoined = errors.New("enrollment already joined")

// saidError is a message in the reader's language standing for a sentinel
// callers match with errors.Is; the sentinel's own text is never shown.
type saidError struct {
	message  string
	sentinel error
}

func (e saidError) Error() string { return e.message }
func (e saidError) Unwrap() error { return e.sentinel }

// validPeerWorkspace accepts an absolute remote path or one under the
// remote home. The remote machine judges the place itself at import time,
// with the same rules the desktop applies to its own workspace.
func validPeerWorkspace(text i18n.Catalog, dir string) error {
	if len(dir) > 512 || strings.ContainsAny(dir, "\r\n\x00\t") {
		return errors.New(text.T(i18n.DesktopWorkspaceInvalid))
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return errors.New(text.T(i18n.ClusterWorkspaceAbsolute))
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return errors.New(text.T(i18n.ClusterWorkspaceParent))
		}
	}
	return nil
}

func (p *Peer) validateJoiningNetwork(ctx context.Context, candidate coordination.Member) error {
	state := p.Runtime.Load().Status().State
	peers := make([]coordination.Member, 0, len(state.Voters)+1)
	for id, member := range state.Members {
		if state.Voters[id] != "" || id == candidate.NodeID {
			peers = append(peers, member)
		}
	}
	return p.validatePeerMesh(ctx, peers)
}

func (p *Peer) authorizeLedgerReplica(ctx context.Context, candidate coordination.Member) error {
	// Also asked by the consensus layer, where no one names a language.
	text := p.text.For(ctx)
	if candidate.StorageLevel != "restricted" && candidate.StorageLevel != "sealed" {
		return errors.New(text.T(i18n.ClusterLedgerNotAllowed))
	}
	runtime := p.Runtime.Load()
	if runtime == nil {
		return coordination.ErrUnavailable
	}
	state, err := runtime.ReadState(ctx)
	if err != nil {
		return err
	}
	for {
		version, err := runtime.Ledger().ReplicaVersion()
		if err != nil {
			return err
		}
		if version >= state.AppVersion {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	declaration, found, err := platformconfig.New(runtime.Ledger()).Load()
	if err != nil {
		return err
	}
	if !found {
		// Before the first application publishes shared declarations, only
		// the explicit local configuration can establish the ledger boundary.
		local, err := config.Load(p.Options.ConfigPath)
		if err != nil {
			return err
		}
		for _, project := range local.Projects {
			if project.Level == "sealed" {
				return errors.New(text.T(i18n.ClusterSealedProject))
			}
		}
		return nil
	}
	for _, item := range declaration.Projects {
		if item.Level == "sealed" {
			return errors.New(text.T(i18n.ClusterSealedProject))
		}
	}
	return nil
}

func (p *Peer) validatePeerMesh(ctx context.Context, peers []coordination.Member) error {
	text := p.text.For(ctx)
	for _, source := range peers {
		for {
			var result networkCheckResult
			if err := p.peerJSON(ctx, source, http.MethodPost, "/cluster/network/check", networkCheckRequest{Peers: peers}, &result); err != nil {
				return fmt.Errorf(text.T(i18n.ClusterMeshFailed), source.NodeID, err)
			}
			if result.Ready {
				break
			}
			if !result.Synchronizing {
				return errors.New(text.T(i18n.ClusterMeshNotReady, source.NodeID, result.Error))
			}
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

type networkCheckRequest struct {
	Peers []coordination.Member `json:"peers"`
}

type networkCheckResult struct {
	Ready         bool   `json:"ready"`
	Error         string `json:"error,omitempty"`
	Synchronizing bool   `json:"synchronizing,omitempty"`
}

func (p *Peer) serveNetworkCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !p.authorizedPeerRequest(r, "network-check") {
		http.Error(w, "owner authorization required", http.StatusForbidden)
		return
	}
	var request networkCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// The asking node wraps this answer in a sentence for whoever is
	// enrolling a machine, in their language; with no one named, the Hub's.
	text := p.text
	if locale := i18n.LocaleFromHeader(r.Header.Get("Accept-Language")); locale != "" {
		text = i18n.New(locale)
	}
	state := p.Runtime.Load().Status().State
	transportPeers := p.Runtime.Load().TransportPeers()
	for _, member := range request.Peers {
		known, ok := state.Members[member.NodeID]
		if !ok || known.Address != member.Address || known.APIAddress != member.APIAddress || transportPeers[member.NodeID] != member.Address {
			WriteJSON(w, networkCheckResult{Error: text.T(i18n.ClusterMemberAddressPending), Synchronizing: true})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, err := p.client.Probe(ctx, member)
		if err == nil {
			var tlsConfig *tls.Config
			tlsConfig, err = p.identity.ClientConfig(member.NodeID)
			if err == nil {
				var connection net.Conn
				connection, err = p.peerDial(member.NodeID, true, 3*time.Second)(ctx, "tcp", member.Address)
				if err == nil {
					secured := tls.Client(connection, tlsConfig)
					err = secured.HandshakeContext(ctx)
					secured.Close()
				}
			}
		}
		cancel()
		if err != nil {
			WriteJSON(w, networkCheckResult{Error: text.T(i18n.ClusterNodeUnreachable, member.NodeID)})
			return
		}
	}
	WriteJSON(w, networkCheckResult{Ready: true})
}

func (p *Peer) authorizedPeerRequest(r *http.Request, action string) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	identity, err := coordination.CertificateIdentity(r.TLS.PeerCertificates[0])
	if err != nil {
		return false
	}
	_, err = p.authorizeControl(r, identity, action)
	return err == nil
}

func (p *Peer) peerJSON(ctx context.Context, member coordination.Member, method, path string, input, output any) error {
	transport, origin, err := p.remoteTransport(member)
	if err != nil {
		return err
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(data))
	}
	request, err := http.NewRequestWithContext(ctx, method, origin.String()+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.OwnerToken)
	request.Header.Set("Content-Type", "application/json")
	if locale := i18n.ContextLocale(ctx); locale != "" {
		// Asked on someone's behalf: the answer is theirs to read.
		request.Header.Set("Accept-Language", string(locale))
	}
	response, err := (&http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("peer HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(output)
}

func (p *Peer) servePeerEnrollment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !p.authorizedPeerRequest(r, "enrollment") {
		http.Error(w, "owner authorization required", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	switch strings.TrimPrefix(r.URL.Path, "/cluster/enrollment/") {
	case "prepare":
		var request struct {
			ID      string                `json:"id"`
			Request PeerEnrollmentRequest `json:"request"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		bundle, err := p.PreparePeerEnrollment(r.Context(), request.Request, request.ID)
		if err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, struct {
			NodeID  string `json:"node_id"`
			Payload []byte `json:"payload"`
		}{bundle.NodeID, bundle.Payload})
	case "complete":
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		result, err := p.CompletePeerEnrollment(r.Context(), request.ID)
		if err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, result)
	case "register-worker":
		var request struct {
			ID     string `json:"operation_id"`
			NodeID string `json:"node_id"`
			Level  string `json:"level"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := p.RegisterEnrolledWorker(r.Context(), request.NodeID, request.Level); err != nil {
			HTTPError(w, err)
			return
		}
		WriteJSON(w, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (p *Peer) RegisterEnrolledWorker(ctx context.Context, nodeID, level string) error {
	active, err := p.Runtime.Load().WaitReady(ctx)
	if err != nil {
		return err
	}
	state, err := p.Runtime.Load().ReadState(ctx)
	if err != nil {
		return err
	}
	member, ok := state.Members[nodeID]
	if !ok || state.Replicas[nodeID] == "" {
		return coordination.ErrNotReady
	}
	worker, err := p.FetchWorker(ctx, member)
	if err != nil {
		return err
	}
	p.Mu.RLock()
	var admin *adminsvc.Service
	if p.Application != nil && p.Application.Generation == active.Generation {
		admin = p.Application.Admin
	}
	p.Mu.RUnlock()
	if admin == nil {
		return coordination.ErrNotReady
	}
	return admin.AdmitWorker(ctx, nodeID, node.Config{Addr: worker.Address, Token: worker.Token, Level: level, DialContext: p.DialWorker})
}

type PeerImportResult struct {
	NodeID      string `json:"node_id"`
	ConfigPath  string `json:"config_path"`
	ClusterPath string `json:"cluster_path"`
}

func ImportPeerPackage(data []byte, stateDir string) (PeerImportResult, error) {
	var bundle PeerJoinPackage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return PeerImportResult{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return PeerImportResult{}, errors.New("enrollment package must contain one object")
	}
	canonical, _ := json.Marshal(bundle)
	digest := sha256.Sum256(canonical)
	packageHash := hex.EncodeToString(digest[:])
	if bundle.Version != 1 || bundle.OperationID == "" || bundle.ClusterID == "" || bundle.NodeID == "" || bundle.WorkspaceDir == "" || len(bundle.OwnerToken) < 32 || len(bundle.WorkerToken) < 32 || len(bundle.Seeds) == 0 {
		return PeerImportResult{}, errors.New("incomplete peer enrollment package")
	}
	text := i18n.New(bundle.Locale)
	if bundle.StorageLevel != "restricted" && bundle.StorageLevel != "sealed" {
		return PeerImportResult{}, errors.New(text.T(i18n.ClusterPackageNoLedgerGrant))
	}
	if err := validPeerEndpoint(text, bundle.PeerAdvertise, true); err != nil {
		return PeerImportResult{}, err
	}
	if err := validPeerEndpoint(text, bundle.RaftAdvertise, true); err != nil {
		return PeerImportResult{}, err
	}
	if err := validatePeerCertificate(bundle); err != nil {
		return PeerImportResult{}, err
	}
	for nodeID, route := range bundle.Routes {
		if routed, err := validHubRoute(text, route); err != nil || !routed || nodeID == bundle.NodeID {
			return PeerImportResult{}, errors.New("enrollment package carries an invalid route")
		}
	}

	root, err := filepath.Abs(stateDir)
	if err != nil {
		return PeerImportResult{}, err
	}
	configPath := filepath.Join(root, "config.json")
	clusterPath := DefaultClusterConfigPath(configPath)
	result := PeerImportResult{NodeID: bundle.NodeID, ConfigPath: configPath, ClusterPath: clusterPath}
	markerPath := filepath.Join(root, "join-operation.json")
	if _, err := os.Lstat(root); err == nil {
		var marker struct {
			ID          string `json:"id"`
			ClusterID   string `json:"cluster_id"`
			NodeID      string `json:"node_id"`
			PackageHash string `json:"package_hash"`
		}
		raw, err := ReadClusterPrivate(markerPath)
		if err != nil {
			return result, errors.New("target already contains a different installation")
		}
		if json.Unmarshal(raw, &marker) != nil || marker.ID != bundle.OperationID || marker.ClusterID != bundle.ClusterID || marker.NodeID != bundle.NodeID || marker.PackageHash != packageHash {
			return result, errors.New("target belongs to a different enrollment operation")
		}
		if _, err := LoadClusterPeerConfig(clusterPath); err != nil {
			return result, err
		}
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return result, err
	}
	// The workspace is judged and created here, on the machine that owns
	// it: the owner's answer may name places this machine refuses.
	workspace, err := desktop.PrepareWorkspace(text, bundle.WorkspaceDir, root)
	if err != nil {
		return result, fmt.Errorf("workspace %q: %w", bundle.WorkspaceDir, err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(root), ".peer-import-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	clusterDir := filepath.Join(root, "cluster")
	settings := PeerConfig{Version: 1, ClusterID: bundle.ClusterID, NodeID: bundle.NodeID, StorageLevel: bundle.StorageLevel, Name: bundle.Name, DataDir: clusterDir, RaftAddress: bundle.RaftAdvertise, PeerAddress: bundle.PeerAdvertise, RaftBindAddress: bundle.RaftListen, PeerBindAddress: bundle.PeerListen, PeerURL: "https://" + bundle.PeerAdvertise, UIAddress: "127.0.0.1:0", CACertFile: filepath.Join(clusterDir, "ca.pem"), CertFile: filepath.Join(clusterDir, "node.pem"), KeyFile: filepath.Join(clusterDir, "node-key.pem"), OwnerTokenFile: filepath.Join(clusterDir, "owner-control-token"), WorkerConfigFile: filepath.Join(clusterDir, "node.json"), Seeds: bundle.Seeds, Routes: bundle.Routes}
	UIToken := ClusterRandomToken()
	disabled := false
	app := config.Config{Agents: map[string]config.Agent{}, Harnesses: map[string]config.Harness{}, MCPServers: map[string]config.MCPServer{}, Projects: map[string]config.Project{"workspace": {Home: config.ProjectHome{Path: workspace}}}, Feishu: config.Feishu{Enabled: &disabled}, Gateway: config.Gateway{HubID: bundle.NodeID, OwnerID: "owner-" + bundle.NodeID, Locale: string(text.Locale()), DefaultChannel: "console", StatePath: filepath.Join(root, "state.json"), HomePath: filepath.Join(root, "home"), ReadModelAddr: "127.0.0.1:0", ReadModelToken: UIToken, PromptTimeout: config.Duration(10 * time.Minute)}}
	worker := node.ServerConfig{Name: bundle.NodeID, Listen: "127.0.0.1:0", Token: bundle.WorkerToken, Hubs: map[string]string{bundle.ClusterID: bundle.WorkerToken}, StateDir: filepath.Join(clusterDir, "node"), StateRoot: root, WorkspaceRoot: workspace, Harnesses: map[string]node.HarnessSpec{}}
	for _, dir := range []string{"cluster", "home"} {
		if err := os.MkdirAll(filepath.Join(staging, dir), 0o700); err != nil {
			return result, err
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"cluster/ca.pem", bundle.CA}, {"cluster/node.pem", bundle.Certificate}, {"cluster/node-key.pem", bundle.PrivateKey}, {"cluster/owner-control-token", []byte(bundle.OwnerToken)}} {
		if err := WritePrivate(filepath.Join(staging, file.name), file.data, true); err != nil {
			return result, err
		}
	}
	for _, file := range []struct {
		name  string
		value any
	}{{"config.json", app}, {"config.json.cluster.json", settings}, {"cluster/node.json", worker}, {"join-operation.json", map[string]string{"id": bundle.OperationID, "cluster_id": bundle.ClusterID, "node_id": bundle.NodeID, "package_hash": packageHash}}} {
		if err := SaveClusterJSON(filepath.Join(staging, file.name), file.value, true); err != nil {
			return result, err
		}
	}
	if err := os.Rename(staging, root); err != nil {
		return result, err
	}
	return result, fsx.SyncDir(filepath.Dir(root))
}

func validatePeerCertificate(bundle PeerJoinPackage) error {
	pair, err := tls.X509KeyPair(bundle.Certificate, bundle.PrivateKey)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle.CA) {
		return errors.New("invalid cluster CA")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}); err != nil {
		return err
	}
	identity, err := coordination.CertificateIdentity(leaf)
	if err != nil || identity != (coordination.Identity{ClusterID: bundle.ClusterID, NodeID: bundle.NodeID}) {
		return errors.New("peer certificate identity differs from enrollment")
	}
	return nil
}
