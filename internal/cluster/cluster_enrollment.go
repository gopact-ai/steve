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
	"maps"
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
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/platformconfig"
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

func validPeerEndpoint(address string, allowLoopback bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("地址需要包含明确的主机和端口")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || host == "" {
		return errors.New("节点端口必须在 1 到 65535 之间")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || !allowLoopback && ip.IsLoopback() {
			return errors.New("跨机接入需要可达的 LAN 地址，不能使用通配或本地回环地址")
		}
	} else if strings.ContainsAny(host, "/\\\x00\r\n\t ") || !allowLoopback && strings.EqualFold(host, "localhost") {
		return errors.New("节点主机地址不合法")
	}
	return nil
}

func (p *Peer) PreviewPeerEnrollment(ctx context.Context, request PeerEnrollmentRequest) (PeerEnrollmentPlan, error) {
	return p.PreviewEnrollment(ctx, request, false)
}

func (p *Peer) PreviewEnrollment(ctx context.Context, request PeerEnrollmentRequest, allowLoopback bool) (PeerEnrollmentPlan, error) {
	request.ExpectedPlanHash = ""
	name, err := coordination.MemberName(request.Name)
	if err != nil {
		return PeerEnrollmentPlan{}, errors.New("机器名称需要 1–64 个字符，不含控制字符")
	}
	request.Name = name
	if request.WorkspaceDir = strings.TrimSpace(request.WorkspaceDir); request.WorkspaceDir == "" {
		request.WorkspaceDir = DefaultPeerWorkspace
	}
	if err := validPeerWorkspace(request.WorkspaceDir); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if err := validPeerEndpoint(request.PeerAddress, allowLoopback); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	host, port, _ := net.SplitHostPort(request.PeerAddress)
	if request.RaftAddress == "" {
		n, _ := strconv.Atoi(port)
		if n >= 65535 {
			return PeerEnrollmentPlan{}, errors.New("需要另行指定共识端口")
		}
		request.RaftAddress = net.JoinHostPort(host, strconv.Itoa(n+1))
	}
	if err := validPeerEndpoint(request.RaftAddress, allowLoopback); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if request.PeerAddress == request.RaftAddress {
		return PeerEnrollmentPlan{}, errors.New("HTTPS 与共识端口必须不同")
	}
	routed, err := validHubRoute(request.HubRoute)
	if err != nil {
		return PeerEnrollmentPlan{}, err
	}
	level := datalevel.Level(request.Level).OrDefault()
	if level != datalevel.Public && level != datalevel.Internal && level != datalevel.Restricted && level != datalevel.Sealed {
		return PeerEnrollmentPlan{}, errors.New("节点数据等级不合法")
	}
	request.Level = string(level)
	if request.Level != "restricted" && request.Level != "sealed" {
		return PeerEnrollmentPlan{}, errors.New("完整机群节点会保存私有任务、会话和记忆账本，需要明确选择 restricted 或 sealed 保存授权；低等级节点只能使用独立执行节点入口")
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
		return PeerEnrollmentPlan{}, errors.New("本机尚未加入机群")
	}
	if p.Config.CAKeyFile == "" {
		return PeerEnrollmentPlan{}, errors.New("请从保管机群签发密钥的原始 App 接入新节点")
	}
	plan := PeerEnrollmentPlan{Request: request, ClusterID: state.ClusterID}
	for id, member := range state.Members {
		if member.Address == request.RaftAddress || member.APIAddress == "https://"+request.PeerAddress {
			return PeerEnrollmentPlan{}, errors.New("目标端口已属于另一个机群成员")
		}
		if state.Voters[id] == "" {
			continue
		}
		// The machine reaches this node through the session, whatever this
		// node advertises; the other members it must reach on its own.
		if id != p.Config.NodeID || !routed {
			if err := validPeerEndpoint(member.Address, allowLoopback); err != nil {
				return PeerEnrollmentPlan{}, fmt.Errorf("节点 %s 尚未设置跨机可达地址：%w", member.NodeID, err)
			}
			endpoint, err := url.Parse(member.APIAddress)
			if err != nil || endpoint.Scheme != "https" {
				return PeerEnrollmentPlan{}, errors.New("机群成员缺少 HTTPS 地址")
			}
			if err := validPeerEndpoint(endpoint.Host, allowLoopback); err != nil {
				return PeerEnrollmentPlan{}, err
			}
		}
		plan.Seeds = append(plan.Seeds, member)
	}
	sort.Slice(plan.Seeds, func(i, j int) bool { return plan.Seeds[i].NodeID < plan.Seeds[j].NodeID })
	if routed {
		plan.Effects = append(plan.Effects, fmt.Sprintf("两台机器经由这次接入的 SSH 会话互联：本机在目标机上以 %s（共识）和 %s（HTTPS）出现，目标机在本机上以回环端口出现；无论本机在哪个网络、是否开着 VPN，都不需要能直接路由到对方", request.HubRoute.Raft, request.HubRoute.API))
	}
	plan.Effects = append(plan.Effects, fmt.Sprintf("在目标机启动持久节点：HTTPS %s，共识 %s", request.PeerAddress, request.RaftAddress), fmt.Sprintf("在目标机创建工作目录 %s，作为这台机器的默认项目目录和执行目录", request.WorkspaceDir), "创建独立节点身份、机群证书和空执行服务；不会复制 Agent 登录状态", "将复制完整私有协作账本，包括任务、会话、工作配置和记忆；该节点已明确获准保存 restricted 级别数据", "先复制协作数据并验证节点间双向连接，再加入投票成员；默认不允许自动晋升", "仅在数据同步和执行服务登记完成后标记接入成功")
	plan.ReviewID = plan.reviewHash()
	return plan, nil
}

func (plan PeerEnrollmentPlan) reviewHash() string {
	plan.ReviewID = ""
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
	if strings.TrimSpace(id) == "" {
		return PeerEnrollmentPackage{}, errors.New("入组操作 ID 不能为空")
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
		return PeerEnrollmentPackage{}, fmt.Errorf("%w: 接入网络计划已改变，请重新审阅", coordination.ErrConflict)
	}
	caPEM, err := ReadClusterPrivate(p.Config.CACertFile)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return PeerEnrollmentPackage{}, errors.New("机群证书无法读取")
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
		return PeerEnrollmentPackage{}, errors.New("机群签发密钥无法读取")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return PeerEnrollmentPackage{}, errors.New("不支持的机群签发密钥类型")
	}
	nodeID := clusterRandomID("node-")
	certificate, leafKey, err := IssueNodeCertificate(ca, privateKey, p.Config.ClusterID, nodeID)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	workerToken := ClusterRandomToken()
	_, peerPort, _ := net.SplitHostPort(plan.Request.PeerAddress)
	_, raftPort, _ := net.SplitHostPort(plan.Request.RaftAddress)
	bundle := PeerJoinPackage{Version: 1, OperationID: id, ClusterID: p.Config.ClusterID, NodeID: nodeID, StorageLevel: plan.Request.Level, Name: plan.Request.Name, WorkspaceDir: plan.Request.WorkspaceDir, PeerListen: net.JoinHostPort("0.0.0.0", peerPort), PeerAdvertise: plan.Request.PeerAddress, RaftListen: net.JoinHostPort("0.0.0.0", raftPort), RaftAdvertise: plan.Request.RaftAddress, CA: caPEM, Certificate: certificate, PrivateKey: leafKey, OwnerToken: p.OwnerToken, WorkerToken: workerToken, Seeds: plan.Seeds}
	if plan.Request.HubRoute.Raft != "" {
		bundle.Routes = map[string]coordination.Route{p.Config.NodeID: plan.Request.HubRoute}
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	record := peerEnrollmentRecord{PeerEnrollmentResult: PeerEnrollmentResult{OperationID: id, NodeID: nodeID, Name: plan.Request.Name, Phase: "prepared", Steps: []PeerEnrollmentStep{{At: time.Now().UTC(), Stage: "prepared", Message: "用户已确认接入，独立节点身份和私有入组包已生成"}}}, Request: plan.Request, Fingerprint: fingerprint, Package: payload, Plan: plan}
	if err := p.saveEnrollment(record); err != nil {
		return PeerEnrollmentPackage{}, err
	}
	return PeerEnrollmentPackage{NodeID: nodeID, Payload: payload, Plan: plan}, nil
}

// validHubRoute accepts a route that is either absent or a pair of
// loopback host:port addresses on the machine. It answers whether the
// enrollment is routed.
func validHubRoute(route coordination.Route) (bool, error) {
	if route.Raft == "" && route.API == "" {
		return false, nil
	}
	if err := loopbackRoute(route); err != nil {
		return false, err
	}
	return true, nil
}

// loopbackRoute accepts two distinct loopback host:port addresses with
// real ports: a tunnel's ends are fixed ports, never 0.
func loopbackRoute(route coordination.Route) error {
	for _, address := range []string{route.Raft, route.API} {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return errors.New("SSH 隧道端口需要写成 host:port")
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return errors.New("SSH 隧道端口必须在目标机的回环地址上")
		}
		if number, err := strconv.Atoi(port); err != nil || number <= 0 || number > 65535 {
			return errors.New("SSH 隧道端口必须是 1 到 65535 之间的固定端口")
		}
	}
	if route.Raft == route.API {
		return errors.New("SSH 隧道的共识与 HTTPS 端口必须不同")
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
		return finish("awaiting_peer", fmt.Errorf("等待新节点启动并提供机群 HTTPS 服务：%w", err))
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
			err = errors.New("执行服务登记未确认")
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
			return finish("synchronizing", fmt.Errorf("%w：协作数据复制中，节点已应用 %d/%d", coordination.ErrNotReady, progress.AppliedIndex, state.AppliedIndex))
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
	record, err := p.loadEnrollment(id)
	if errors.Is(err, os.ErrNotExist) {
		return ErrEnrollmentGone
	}
	if err != nil {
		return err
	}
	if record.Ready {
		return ErrEnrollmentJoined
	}
	if err := p.leaveCluster(ctx, id+"/abandon", record.NodeID); err != nil {
		return fmt.Errorf("收回这次接入失败：%w", err)
	}
	path := p.enrollmentPath(id)
	archived := path + ".abandoned-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(path, archived); err != nil {
		return fmt.Errorf("归档接入记录失败：%w", err)
	}
	return nil
}

// DefaultPeerWorkspace is where a machine keeps its work unless the owner
// chooses somewhere: a visible directory under the remote account's home.
const DefaultPeerWorkspace = sshconnect.DefaultWorkspaceDir

// ErrEnrollmentGone says there is no such enrollment to give up; a caller
// that only knows the operation ID has nothing left to do.
var ErrEnrollmentGone = errors.New("这次接入的记录已不存在，无需放弃")

// ErrEnrollmentJoined says the machine is a member now: leaving the cluster
// is its own reviewed action on the resources page, not an enrollment undo.
var ErrEnrollmentJoined = errors.New("这台机器已经完成接入，请在资源页移除该成员")

// validPeerWorkspace accepts an absolute remote path or one under the
// remote home. The remote machine judges the place itself at import time,
// with the same rules the desktop applies to its own workspace.
func validPeerWorkspace(dir string) error {
	if len(dir) > 512 || strings.ContainsAny(dir, "\r\n\x00\t") {
		return errors.New("工作目录包含无效字符")
	}
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return errors.New("工作目录要写目标机上的绝对路径，或以 ~/ 开头，例如 ~/steve-workspace")
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." {
			return errors.New("工作目录不能包含 ..")
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
	if candidate.StorageLevel != "restricted" && candidate.StorageLevel != "sealed" {
		return errors.New("完整节点没有获准保存私有协作账本")
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
				return errors.New("密封项目不能进入完整复制账本")
			}
		}
		return nil
	}
	for _, item := range declaration.Projects {
		if item.Level == "sealed" {
			return errors.New("密封项目不能进入完整复制账本")
		}
	}
	return nil
}

func (p *Peer) validateMemberAddress(ctx context.Context, proposed coordination.Member) error {
	state := p.Runtime.Load().Status().State
	var peers []coordination.Member
	for id, member := range state.Members {
		if state.Voters[id] == "" {
			continue
		}
		if id == proposed.NodeID {
			member = proposed
		}
		peers = append(peers, member)
	}
	return p.validatePeerMesh(ctx, peers)
}

func (p *Peer) validatePeerMesh(ctx context.Context, peers []coordination.Member) error {
	for _, source := range peers {
		for {
			var result networkCheckResult
			if err := p.peerJSON(ctx, source, http.MethodPost, "/cluster/network/check", networkCheckRequest{Peers: peers}, &result); err != nil {
				return fmt.Errorf("节点 %s 无法完成独立互联验证：%w", source.NodeID, err)
			}
			if result.Ready {
				break
			}
			if !result.Synchronizing {
				return fmt.Errorf("节点 %s 的独立连接尚未就绪：%s", source.NodeID, result.Error)
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
	state := p.Runtime.Load().Status().State
	transportPeers := p.Runtime.Load().TransportPeers()
	for _, member := range request.Peers {
		known, ok := state.Members[member.NodeID]
		pending, preparing := state.PendingAddresses[member.NodeID]
		proposed := preparing && pending.Address == member.Address && pending.APIAddress == member.APIAddress
		if !ok || !proposed && (known.Address != member.Address || known.APIAddress != member.APIAddress || transportPeers[member.NodeID] != member.Address) {
			WriteJSON(w, networkCheckResult{Error: "成员地址尚未同步", Synchronizing: true})
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
			WriteJSON(w, networkCheckResult{Error: "无法独立连接节点 " + member.NodeID})
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
	admin.Mu.Lock()
	defer admin.Mu.Unlock()
	adminsvc.ConfigMu.Lock()
	old := admin.Cfg.Nodes
	updated := maps.Clone(old)
	if updated == nil {
		updated = map[string]config.Node{}
	}
	next := config.Node{Addr: worker.Address, Token: worker.Token, Level: string(datalevel.Level(level).OrDefault())}
	if existing, ok := updated[nodeID]; ok {
		if existing.Addr != next.Addr || existing.Token != next.Token {
			adminsvc.ConfigMu.Unlock()
			return coordination.ErrConflict
		}
		next = existing
	}
	if _, ok := updated[nodeID]; !ok {
		updated[nodeID] = next
		admin.Cfg.Nodes = updated
		if err := admin.PersistConfig(admin.Cfg); err != nil {
			admin.Cfg.Nodes = old
			adminsvc.ConfigMu.Unlock()
			return err
		}
	}
	levels, regions := admin.Cfg.NodeLevels(), admin.Cfg.NodeRegions()
	adminsvc.ConfigMu.Unlock()
	admin.Nodes.Add(nodeID, node.Config{Addr: worker.Address, Token: worker.Token, Level: next.Level, DialContext: p.DialWorker})
	admin.Fleet.SetNodeLevels(levels)
	admin.Fleet.SetNodeRegions(regions)
	_, err = admin.Nodes.Refresh(ctx, nodeID)
	return err
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
	if bundle.StorageLevel != "restricted" && bundle.StorageLevel != "sealed" {
		return PeerImportResult{}, errors.New("入组包没有明确授权私有协作账本存储")
	}
	if err := validPeerEndpoint(bundle.PeerAdvertise, true); err != nil {
		return PeerImportResult{}, err
	}
	if err := validPeerEndpoint(bundle.RaftAdvertise, true); err != nil {
		return PeerImportResult{}, err
	}
	if err := validatePeerCertificate(bundle); err != nil {
		return PeerImportResult{}, err
	}
	for nodeID, route := range bundle.Routes {
		if routed, err := validHubRoute(route); err != nil || !routed || nodeID == bundle.NodeID {
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
	workspace, err := desktop.PrepareWorkspace(bundle.WorkspaceDir, root)
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
	app := config.Config{Agents: map[string]config.Agent{}, Harnesses: map[string]config.Harness{}, MCPServers: map[string]config.MCPServer{}, Projects: map[string]config.Project{"workspace": {Home: config.ProjectHome{Path: workspace}}}, Feishu: config.Feishu{Enabled: &disabled}, Gateway: config.Gateway{HubID: bundle.NodeID, OwnerID: "owner-" + bundle.NodeID, Locale: "zh", DefaultChannel: "console", StatePath: filepath.Join(root, "state.json"), HomePath: filepath.Join(root, "home"), ReadModelAddr: "127.0.0.1:0", ReadModelToken: UIToken, PromptTimeout: config.Duration(10 * time.Minute)}}
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
