package main

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
	"flag"
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
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/project"
)

type PeerEnrollmentRequest struct {
	Alias            string `json:"alias"`
	Name             string `json:"name"`
	PeerAddress      string `json:"peer_address"`
	RaftAddress      string `json:"raft_address"`
	SourceHost       string `json:"source_host,omitempty"`
	Level            string `json:"level,omitempty"`
	ExpectedPlanHash string `json:"expected_plan_hash,omitempty"`
}

type PeerEnrollmentPlan struct {
	Request             PeerEnrollmentRequest `json:"request"`
	ClusterID           string                `json:"cluster_id"`
	Source              coordination.Member   `json:"source"`
	Seeds               []coordination.Member `json:"seeds"`
	UpdateSourceAddress bool                  `json:"update_source_address"`
	Effects             []string              `json:"effects"`
	PreviousSource      coordination.Member   `json:"previous_source"`
	ReviewID            string                `json:"review_id"`
}

// PeerEnrollmentPackage is kept between the owner service and SSH stdin. It is
// deliberately not serialized into a UI installation plan.
type PeerEnrollmentPackage struct {
	NodeID  string             `json:"-"`
	Payload []byte             `json:"-"`
	Plan    PeerEnrollmentPlan `json:"-"`
}

type peerJoinPackage struct {
	Version       int                   `json:"version"`
	OperationID   string                `json:"operation_id"`
	ClusterID     string                `json:"cluster_id"`
	NodeID        string                `json:"node_id"`
	Name          string                `json:"name"`
	StorageLevel  string                `json:"storage_level"`
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
	Request        PeerEnrollmentRequest              `json:"request"`
	Fingerprint    string                             `json:"fingerprint"`
	Package        []byte                             `json:"package"`
	Plan           PeerEnrollmentPlan                 `json:"plan"`
	SourceRequest  *coordination.MemberAddressRequest `json:"source_request,omitempty"`
	SourceAttempts int                                `json:"source_attempts"`
	SourceReady    bool                               `json:"source_ready"`
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
		_, port, _ = net.SplitHostPort(bound)
	}
	return net.JoinHostPort(host, port)
}

func localAdvertiseAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addresses []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		entries, _ := iface.Addrs()
		for _, entry := range entries {
			ip, _, err := net.ParseCIDR(entry.String())
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.To4() == nil {
				continue
			}
			addresses = append(addresses, ip.String())
		}
	}
	sort.Slice(addresses, func(i, j int) bool {
		a, b := net.ParseIP(addresses[i]), net.ParseIP(addresses[j])
		if a.IsPrivate() != b.IsPrivate() {
			return a.IsPrivate()
		}
		return addresses[i] < addresses[j]
	})
	return addresses
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

func (p *clusterPeer) PreviewPeerEnrollment(ctx context.Context, request PeerEnrollmentRequest) (PeerEnrollmentPlan, error) {
	return p.previewPeerEnrollment(ctx, request, false)
}

func (p *clusterPeer) previewPeerEnrollment(ctx context.Context, request PeerEnrollmentRequest, allowLoopback bool) (PeerEnrollmentPlan, error) {
	request.ExpectedPlanHash = ""
	request.Name = strings.TrimSpace(request.Name)
	request.SourceHost = strings.TrimSpace(request.SourceHost)
	if !adminsvc.NameShape.MatchString(request.Name) {
		return PeerEnrollmentPlan{}, errors.New("节点名称只能包含小写字母、数字、点、下划线和连字符")
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
	level := project.Level(request.Level).OrDefault()
	if level != project.LevelPublic && level != project.LevelInternal && level != project.LevelRestricted && level != project.LevelSealed {
		return PeerEnrollmentPlan{}, errors.New("节点数据等级不合法")
	}
	request.Level = string(level)
	if request.Level != "restricted" && request.Level != "sealed" {
		return PeerEnrollmentPlan{}, errors.New("完整机群节点会保存私有任务、会话和记忆账本，需要明确选择 restricted 或 sealed 保存授权；低等级节点只能使用独立执行节点入口")
	}
	runtime := p.runtime.Load()
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
	source, ok := state.Members[p.config.NodeID]
	if !ok {
		return PeerEnrollmentPlan{}, errors.New("本机尚未加入机群")
	}
	if request.SourceHost == "" {
		request.SourceHost, _, _ = net.SplitHostPort(source.Address)
		if !allowLoopback && net.ParseIP(request.SourceHost).IsLoopback() {
			if choices := localAdvertiseAddresses(); len(choices) > 0 {
				request.SourceHost = choices[0]
			}
		}
	}
	_, raftPort, _ := net.SplitHostPort(source.Address)
	sourceURL, _ := url.Parse(source.APIAddress)
	if sourceURL == nil {
		return PeerEnrollmentPlan{}, errors.New("本机没有可用的 HTTPS 地址")
	}
	proposed := source
	proposed.Address = net.JoinHostPort(request.SourceHost, raftPort)
	proposed.APIAddress = "https://" + net.JoinHostPort(request.SourceHost, sourceURL.Port())
	if err := validPeerEndpoint(proposed.Address, allowLoopback); err != nil {
		return PeerEnrollmentPlan{}, err
	}
	if p.config.CAKeyFile == "" {
		return PeerEnrollmentPlan{}, errors.New("请从保管机群签发密钥的原始 App 接入新节点")
	}
	plan := PeerEnrollmentPlan{Request: request, ClusterID: state.ClusterID, Source: proposed, PreviousSource: source, UpdateSourceAddress: source.Address != proposed.Address || source.APIAddress != proposed.APIAddress}
	for id, member := range state.Members {
		if member.Name == request.Name {
			return PeerEnrollmentPlan{}, fmt.Errorf("机群中已经有名为 %s 的节点", request.Name)
		}
		if member.Address == request.RaftAddress || member.APIAddress == "https://"+request.PeerAddress {
			return PeerEnrollmentPlan{}, errors.New("目标端口已属于另一个机群成员")
		}
		if state.Voters[id] == "" {
			continue
		}
		if id == p.config.NodeID {
			member = proposed
		}
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
		plan.Seeds = append(plan.Seeds, member)
	}
	sort.Slice(plan.Seeds, func(i, j int) bool { return plan.Seeds[i].NodeID < plan.Seeds[j].NodeID })
	if plan.UpdateSourceAddress {
		plan.Effects = append(plan.Effects, fmt.Sprintf("将本机跨机连接地址更新为 %s 和 %s；本机工作台与执行服务继续运行", proposed.Address, proposed.APIAddress))
	}
	plan.Effects = append(plan.Effects, fmt.Sprintf("在目标机启动持久节点：HTTPS %s，共识 %s", request.PeerAddress, request.RaftAddress), "创建独立节点身份、机群证书和空执行服务；不会复制 Agent 登录状态", "将复制完整私有协作账本，包括任务、会话、工作配置和记忆；该节点已明确获准保存 restricted 级别数据", "先复制协作数据并验证节点间双向连接，再加入投票成员；默认不允许自动晋升", "仅在数据同步和执行服务登记完成后标记接入成功")
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

func (p *clusterPeer) PreparePeerEnrollment(ctx context.Context, request PeerEnrollmentRequest, id string) (PeerEnrollmentPackage, error) {
	return p.preparePeerEnrollment(ctx, request, id, false)
}

func (p *clusterPeer) preparePeerEnrollment(ctx context.Context, request PeerEnrollmentRequest, id string, allowLoopback bool) (PeerEnrollmentPackage, error) {
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
		if err := p.prepareSourceNetwork(ctx, &existing); err != nil {
			return PeerEnrollmentPackage{NodeID: existing.NodeID, Payload: existing.Package, Plan: existing.Plan}, err
		}
		return PeerEnrollmentPackage{NodeID: existing.NodeID, Payload: existing.Package, Plan: existing.Plan}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PeerEnrollmentPackage{}, err
	}
	plan, err := p.previewPeerEnrollment(ctx, request, allowLoopback)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	if request.ExpectedPlanHash == "" || request.ExpectedPlanHash != plan.ReviewID {
		return PeerEnrollmentPackage{}, fmt.Errorf("%w: 接入网络计划已改变，请重新审阅", coordination.ErrConflict)
	}
	caPEM, err := readClusterPrivate(p.config.CACertFile)
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
	keyPEM, err := readClusterPrivate(p.config.CAKeyFile)
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
	nodeID, err := clusterRandomID("node-")
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	certificate, leafKey, err := issueClusterNodeCertificate(ca, privateKey, p.config.ClusterID, nodeID)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	workerToken, err := clusterRandomToken()
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	_, peerPort, _ := net.SplitHostPort(plan.Request.PeerAddress)
	_, raftPort, _ := net.SplitHostPort(plan.Request.RaftAddress)
	bundle := peerJoinPackage{Version: 1, OperationID: id, ClusterID: p.config.ClusterID, NodeID: nodeID, StorageLevel: plan.Request.Level, Name: plan.Request.Name, PeerListen: net.JoinHostPort("0.0.0.0", peerPort), PeerAdvertise: plan.Request.PeerAddress, RaftListen: net.JoinHostPort("0.0.0.0", raftPort), RaftAdvertise: plan.Request.RaftAddress, CA: caPEM, Certificate: certificate, PrivateKey: leafKey, OwnerToken: p.ownerToken, WorkerToken: workerToken, Seeds: plan.Seeds}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return PeerEnrollmentPackage{}, err
	}
	record := peerEnrollmentRecord{PeerEnrollmentResult: PeerEnrollmentResult{OperationID: id, NodeID: nodeID, Name: plan.Request.Name, Phase: "prepared", Steps: []PeerEnrollmentStep{{At: time.Now().UTC(), Stage: "prepared", Message: "用户已确认接入，独立节点身份和私有入组包已生成"}}}, Request: plan.Request, Fingerprint: fingerprint, Package: payload, Plan: plan, SourceReady: !plan.UpdateSourceAddress}
	if err := p.saveEnrollment(record); err != nil {
		return PeerEnrollmentPackage{}, err
	}
	if err := p.prepareSourceNetwork(ctx, &record); err != nil {
		return PeerEnrollmentPackage{NodeID: nodeID, Payload: payload, Plan: plan}, err
	}
	return PeerEnrollmentPackage{NodeID: nodeID, Payload: payload, Plan: plan}, nil
}

func (p *clusterPeer) prepareSourceNetwork(ctx context.Context, record *peerEnrollmentRecord) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = p.prepareSourceNetworkOnce(ctx, record)
		if !errors.Is(err, coordination.ErrConflict) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (p *clusterPeer) prepareSourceNetworkOnce(ctx context.Context, record *peerEnrollmentRecord) error {
	if record.SourceReady {
		return nil
	}
	state, err := p.runtime.Load().ReadState(ctx)
	if err != nil {
		return err
	}
	current := state.Members[p.config.NodeID]
	wanted := record.Plan.Source
	if current.Address == wanted.Address && current.APIAddress == wanted.APIAddress {
		if err := p.persistAdvertisement(current.Address, current.APIAddress); err != nil {
			return err
		}
		record.SourceReady = true
		return p.saveEnrollment(*record)
	}
	if current.Address != record.Plan.PreviousSource.Address || current.APIAddress != record.Plan.PreviousSource.APIAddress {
		return fmt.Errorf("%w: 本机地址已由另一操作改变，请重新审阅", coordination.ErrConflict)
	}
	pending, hasPending := state.PendingAddresses[p.config.NodeID]
	if hasPending && (record.SourceRequest == nil || pending.ID != record.SourceRequest.ID) {
		return fmt.Errorf("%w: 本机另一个地址更新尚未完成", coordination.ErrConflict)
	}
	if !hasPending {
		record.SourceAttempts++
		record.SourceRequest = &coordination.MemberAddressRequest{ID: fmt.Sprintf("%s/source-address/%d", record.OperationID, record.SourceAttempts), Actor: "owner", ExpectedRevision: state.Revision, NodeID: p.config.NodeID, Address: wanted.Address, APIAddress: wanted.APIAddress}
		if err := p.saveEnrollment(*record); err != nil {
			return err
		}
	}
	_, err = p.runtime.Load().UpdateMemberAddress(ctx, *record.SourceRequest)
	if err == nil {
		err = p.persistAdvertisement(wanted.Address, wanted.APIAddress)
	}
	if err != nil {
		record.Phase = "source_network"
		record.Error = err.Error()
		_ = p.saveEnrollment(*record)
		return err
	}
	record.SourceReady = true
	record.Error = ""
	record.Phase = "prepared"
	record.Steps = append(record.Steps, PeerEnrollmentStep{At: time.Now().UTC(), Stage: "source_network", Message: "本机可达地址已更新，工作台和执行服务持续运行"})
	return p.saveEnrollment(*record)
}

func (p *clusterPeer) enrollmentPath(id string) string {
	digest := sha256.Sum256([]byte(id))
	return filepath.Join(p.config.DataDir, "enrollments", hex.EncodeToString(digest[:])+".json")
}

func (p *clusterPeer) loadEnrollment(id string) (peerEnrollmentRecord, error) {
	var record peerEnrollmentRecord
	data, err := readClusterPrivate(p.enrollmentPath(id))
	if err != nil {
		return record, err
	}
	err = json.Unmarshal(data, &record)
	if err == nil && record.OperationID != id {
		err = coordination.ErrCommandConflict
	}
	return record, err
}

func (p *clusterPeer) saveEnrollment(record peerEnrollmentRecord) error {
	if err := os.MkdirAll(filepath.Dir(p.enrollmentPath(record.OperationID)), 0o700); err != nil {
		return err
	}
	return saveClusterJSON(p.enrollmentPath(record.OperationID), record, false)
}

func (p *clusterPeer) CompletePeerEnrollment(ctx context.Context, id string) (PeerEnrollmentResult, error) {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	record, err := p.loadEnrollment(id)
	if err != nil {
		return PeerEnrollmentResult{}, err
	}
	if record.Ready {
		return record.PeerEnrollmentResult, nil
	}
	if err := p.prepareSourceNetwork(ctx, &record); err != nil {
		return record.PeerEnrollmentResult, err
	}
	finish := func(stage string, err error) (PeerEnrollmentResult, error) {
		record.Phase = stage
		if err != nil {
			record.Error = err.Error()
		} else {
			record.Error = ""
		}
		record.Steps = append(record.Steps, PeerEnrollmentStep{At: time.Now().UTC(), Stage: stage, Message: record.Error})
		saveErr := p.saveEnrollment(record)
		return record.PeerEnrollmentResult, errors.Join(err, saveErr)
	}
	member := coordination.Member{NodeID: record.NodeID, Name: record.Name, Address: record.Request.RaftAddress, APIAddress: "https://" + record.Request.PeerAddress, AutoEligible: false, StorageLevel: record.Request.Level}
	status, err := p.client.Status(ctx, member)
	if err != nil {
		return finish("awaiting_peer", fmt.Errorf("等待新节点启动并提供机群 HTTPS 服务：%w", err))
	}
	if !status.Healthy {
		return finish("awaiting_peer", coordination.ErrNotReady)
	}
	_, err = p.runtime.Load().Join(ctx, coordination.JoinRequest{ID: id + "/join", Actor: "owner", Member: member})
	if err != nil {
		return finish("synchronizing", err)
	}
	if _, err := finish("joined", nil); err != nil {
		return record.PeerEnrollmentResult, err
	}
	state, err := p.runtime.Load().ReadState(ctx)
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
	state, err = p.runtime.Load().ReadState(ctx)
	if err != nil {
		return finish("synchronizing", err)
	}
	for {
		progress, err := p.client.Probe(ctx, member)
		if err == nil && progress.AppVersion >= state.AppVersion && progress.AppliedIndex >= state.AppliedIndex {
			break
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

func (p *clusterPeer) PeerEnrollmentStatus(_ context.Context, id string) (PeerEnrollmentResult, error) {
	p.enrollmentMu.Lock()
	defer p.enrollmentMu.Unlock()
	record, err := p.loadEnrollment(id)
	return record.PeerEnrollmentResult, err
}

type NetworkAddressRequest struct {
	ID               string `json:"id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Host             string `json:"host"`
}

func (p *clusterPeer) SetNetworkAddress(ctx context.Context, request NetworkAddressRequest) (coordination.Result, error) {
	if request.ID == "" || request.Host == "" || strings.ContainsAny(request.Host, "/\\\x00\r\n\t ") {
		return coordination.Result{}, coordination.ErrInvalid
	}
	boundHost, _, _ := net.SplitHostPort(p.config.RaftBindAddress)
	boundIP := net.ParseIP(boundHost)
	if boundIP == nil || !boundIP.IsUnspecified() {
		return coordination.Result{}, errors.New("当前共识监听绑定到单一地址，需要先完成明确的监听配置变更")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, request.Host)
	if err != nil || len(addresses) == 0 {
		return coordination.Result{}, errors.New("本机连接地址无法解析")
	}
	local := map[string]bool{}
	for _, address := range localAdvertiseAddresses() {
		local[address] = true
	}
	for _, address := range addresses {
		if !address.IP.IsLoopback() && !local[address.IP.String()] {
			return coordination.Result{}, errors.New("请选择本机网络接口上实际存在的地址")
		}
	}
	if net.ParseIP(request.Host).IsUnspecified() {
		return coordination.Result{}, errors.New("请选择本机网络接口上实际存在的地址")
	}
	state, err := p.runtime.Load().ReadState(ctx)
	if err != nil {
		return coordination.Result{}, err
	}
	member := state.Members[p.config.NodeID]
	_, raftPort, _ := net.SplitHostPort(member.Address)
	peerURL, _ := url.Parse(member.APIAddress)
	if peerURL == nil {
		return coordination.Result{}, coordination.ErrInvalid
	}
	newRaft := net.JoinHostPort(request.Host, raftPort)
	newAPI := "https://" + net.JoinHostPort(request.Host, peerURL.Port())
	result, err := p.runtime.Load().UpdateMemberAddress(ctx, coordination.MemberAddressRequest{ID: request.ID, Actor: "owner", ExpectedRevision: request.ExpectedRevision, NodeID: p.config.NodeID, Address: newRaft, APIAddress: newAPI})
	if err != nil {
		return result, err
	}
	return result, p.persistAdvertisement(newRaft, newAPI)
}

func (p *clusterPeer) persistAdvertisement(newRaft, newAPI string) error {
	p.raftAdvertisement.Store(newRaft)
	p.peerAdvertisement.Store(newAPI)
	endpoint, err := url.Parse(newAPI)
	if err != nil {
		return err
	}
	p.mu.Lock()
	saved := p.config
	saved.RaftAddress = newRaft
	saved.PeerAddress = endpoint.Host
	saved.PeerURL = newAPI
	err = saveClusterJSON(p.options.ClusterPath, saved, false)
	p.mu.Unlock()
	p.client.RememberMembers([]coordination.Member{{NodeID: p.config.NodeID, Name: p.config.Name, Address: newRaft, APIAddress: newAPI}})
	return err
}

func (p *clusterPeer) validateJoiningNetwork(ctx context.Context, candidate coordination.Member) error {
	state := p.runtime.Load().Status().State
	peers := make([]coordination.Member, 0, len(state.Voters)+1)
	for id, member := range state.Members {
		if state.Voters[id] != "" || id == candidate.NodeID {
			peers = append(peers, member)
		}
	}
	return p.validatePeerMesh(ctx, peers)
}

func (p *clusterPeer) authorizeLedgerReplica(ctx context.Context, candidate coordination.Member) error {
	if candidate.StorageLevel != "restricted" && candidate.StorageLevel != "sealed" {
		return errors.New("完整节点没有获准保存私有协作账本")
	}
	runtime := p.runtime.Load()
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
		local, err := config.Load(p.options.ConfigPath)
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

func (p *clusterPeer) validateMemberAddress(ctx context.Context, proposed coordination.Member) error {
	state := p.runtime.Load().Status().State
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

func (p *clusterPeer) validatePeerMesh(ctx context.Context, peers []coordination.Member) error {
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

func (p *clusterPeer) serveNetworkCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !p.authorizedPeerRequest(r, "network-check") {
		http.Error(w, "owner authorization required", http.StatusForbidden)
		return
	}
	var request networkCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	state := p.runtime.Load().Status().State
	transportPeers := p.runtime.Load().TransportPeers()
	for _, member := range request.Peers {
		known, ok := state.Members[member.NodeID]
		pending, preparing := state.PendingAddresses[member.NodeID]
		proposed := preparing && pending.Address == member.Address && pending.APIAddress == member.APIAddress
		if !ok || !proposed && (known.Address != member.Address || known.APIAddress != member.APIAddress || transportPeers[member.NodeID] != member.Address) {
			writePeerJSON(w, networkCheckResult{Error: "成员地址尚未同步", Synchronizing: true})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, err := p.client.Probe(ctx, member)
		if err == nil {
			var tlsConfig *tls.Config
			tlsConfig, err = p.identity.ClientConfig(member.NodeID)
			if err == nil {
				var connection net.Conn
				connection, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", member.Address)
				if connection != nil {
					connection.Close()
				}
			}
		}
		cancel()
		if err != nil {
			writePeerJSON(w, networkCheckResult{Error: "无法独立连接节点 " + member.NodeID})
			return
		}
	}
	writePeerJSON(w, networkCheckResult{Ready: true})
}

func (p *clusterPeer) authorizedPeerRequest(r *http.Request, action string) bool {
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

func (p *clusterPeer) peerJSON(ctx context.Context, member coordination.Member, method, path string, input, output any) error {
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
	request.Header.Set("Authorization", "Bearer "+p.ownerToken)
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

func (p *clusterPeer) servePeerEnrollment(w http.ResponseWriter, r *http.Request) {
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
			peerHTTPError(w, err)
			return
		}
		writePeerJSON(w, struct {
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
			peerHTTPError(w, err)
			return
		}
		writePeerJSON(w, result)
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
		if err := p.registerEnrolledWorker(r.Context(), request.NodeID, request.Level); err != nil {
			peerHTTPError(w, err)
			return
		}
		writePeerJSON(w, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (p *clusterPeer) registerEnrolledWorker(ctx context.Context, nodeID, level string) error {
	active, err := p.runtime.Load().WaitReady(ctx)
	if err != nil {
		return err
	}
	state, err := p.runtime.Load().ReadState(ctx)
	if err != nil {
		return err
	}
	member, ok := state.Members[nodeID]
	if !ok || state.Voters[nodeID] == "" {
		return coordination.ErrNotReady
	}
	worker, err := p.FetchWorker(ctx, member)
	if err != nil {
		return err
	}
	p.mu.RLock()
	var admin *adminsvc.Service
	if p.application != nil && p.application.Generation == active.Generation {
		admin = p.application.Admin
	}
	p.mu.RUnlock()
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
	next := config.Node{Addr: worker.Address, Token: worker.Token, Level: string(project.Level(level).OrDefault())}
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

func peerImportCmd(args []string) error {
	flags := flag.NewFlagSet("peer-import", flag.ContinueOnError)
	packagePath := flags.String("package", "", "private enrollment package")
	stateDir := flags.String("state-dir", "", "new peer state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *packagePath == "" || *stateDir == "" || flags.NArg() != 0 {
		return errors.New("peer-import requires --package and --state-dir")
	}
	data, err := readClusterPrivate(*packagePath)
	if err != nil {
		return err
	}
	result, err := importPeerPackage(data, *stateDir)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

type peerImportResult struct {
	NodeID      string `json:"node_id"`
	ConfigPath  string `json:"config_path"`
	ClusterPath string `json:"cluster_path"`
}

func importPeerPackage(data []byte, stateDir string) (peerImportResult, error) {
	var bundle peerJoinPackage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return peerImportResult{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return peerImportResult{}, errors.New("enrollment package must contain one object")
	}
	canonical, _ := json.Marshal(bundle)
	digest := sha256.Sum256(canonical)
	packageHash := hex.EncodeToString(digest[:])
	if bundle.Version != 1 || bundle.OperationID == "" || bundle.ClusterID == "" || bundle.NodeID == "" || len(bundle.OwnerToken) < 32 || len(bundle.WorkerToken) < 32 || len(bundle.Seeds) == 0 {
		return peerImportResult{}, errors.New("incomplete peer enrollment package")
	}
	if bundle.StorageLevel != "restricted" && bundle.StorageLevel != "sealed" {
		return peerImportResult{}, errors.New("入组包没有明确授权私有协作账本存储")
	}
	if err := validPeerEndpoint(bundle.PeerAdvertise, true); err != nil {
		return peerImportResult{}, err
	}
	if err := validPeerEndpoint(bundle.RaftAdvertise, true); err != nil {
		return peerImportResult{}, err
	}
	pair, err := tls.X509KeyPair(bundle.Certificate, bundle.PrivateKey)
	if err != nil {
		return peerImportResult{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle.CA) {
		return peerImportResult{}, errors.New("invalid cluster CA")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return peerImportResult{}, err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}); err != nil {
		return peerImportResult{}, err
	}
	identity, err := coordination.CertificateIdentity(leaf)
	if err != nil || identity != (coordination.Identity{ClusterID: bundle.ClusterID, NodeID: bundle.NodeID}) {
		return peerImportResult{}, errors.New("peer certificate identity differs from enrollment")
	}
	root, err := filepath.Abs(stateDir)
	if err != nil {
		return peerImportResult{}, err
	}
	configPath := filepath.Join(root, "config.json")
	clusterPath := defaultClusterConfigPath(configPath)
	result := peerImportResult{NodeID: bundle.NodeID, ConfigPath: configPath, ClusterPath: clusterPath}
	markerPath := filepath.Join(root, "join-operation.json")
	if _, err := os.Lstat(root); err == nil {
		var marker struct {
			ID          string `json:"id"`
			ClusterID   string `json:"cluster_id"`
			NodeID      string `json:"node_id"`
			PackageHash string `json:"package_hash"`
		}
		raw, err := readClusterPrivate(markerPath)
		if err != nil {
			return result, errors.New("target already contains a different installation")
		}
		if json.Unmarshal(raw, &marker) != nil || marker.ID != bundle.OperationID || marker.ClusterID != bundle.ClusterID || marker.NodeID != bundle.NodeID || marker.PackageHash != packageHash {
			return result, errors.New("target belongs to a different enrollment operation")
		}
		if _, err := loadClusterPeerConfig(clusterPath); err != nil {
			return result, err
		}
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return result, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(root), ".peer-import-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	clusterDir := filepath.Join(root, "cluster")
	settings := clusterPeerConfig{Version: 1, ClusterID: bundle.ClusterID, NodeID: bundle.NodeID, StorageLevel: bundle.StorageLevel, Name: bundle.Name, DataDir: clusterDir, RaftAddress: bundle.RaftAdvertise, PeerAddress: bundle.PeerAdvertise, RaftBindAddress: bundle.RaftListen, PeerBindAddress: bundle.PeerListen, PeerURL: "https://" + bundle.PeerAdvertise, UIAddress: "127.0.0.1:0", CACertFile: filepath.Join(clusterDir, "ca.pem"), CertFile: filepath.Join(clusterDir, "node.pem"), KeyFile: filepath.Join(clusterDir, "node-key.pem"), OwnerTokenFile: filepath.Join(clusterDir, "owner-control-token"), WorkerConfigFile: filepath.Join(clusterDir, "node.json"), Seeds: bundle.Seeds}
	uiToken, err := clusterRandomToken()
	if err != nil {
		return result, err
	}
	disabled := false
	app := config.Config{Agents: map[string]config.Agent{}, Harnesses: map[string]config.Harness{}, MCPServers: map[string]config.MCPServer{}, Projects: map[string]config.Project{"workspace": {Home: config.ProjectHome{Path: filepath.Join(root, "workspace")}}}, Feishu: config.Feishu{Enabled: &disabled}, Gateway: config.Gateway{HubID: bundle.NodeID, OwnerID: "owner-" + bundle.NodeID, Locale: "zh", DefaultChannel: "console", StatePath: filepath.Join(root, "state.json"), HomePath: filepath.Join(root, "home"), ReadModelAddr: "127.0.0.1:0", ReadModelToken: uiToken, PromptTimeout: config.Duration(10 * time.Minute)}}
	worker := node.ServerConfig{Name: bundle.NodeID, Listen: "127.0.0.1:0", Token: bundle.WorkerToken, Hubs: map[string]string{bundle.ClusterID: bundle.WorkerToken}, StateDir: filepath.Join(clusterDir, "node"), WorkspaceRoot: root, Harnesses: map[string]node.HarnessSpec{}}
	for _, dir := range []string{"cluster", "workspace", "home"} {
		if err := os.MkdirAll(filepath.Join(staging, dir), 0o700); err != nil {
			return result, err
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"cluster/ca.pem", bundle.CA}, {"cluster/node.pem", bundle.Certificate}, {"cluster/node-key.pem", bundle.PrivateKey}, {"cluster/owner-control-token", []byte(bundle.OwnerToken)}} {
		if err := writeClusterPrivate(filepath.Join(staging, file.name), file.data, true); err != nil {
			return result, err
		}
	}
	for _, file := range []struct {
		name  string
		value any
	}{{"config.json", app}, {"config.json.cluster.json", settings}, {"cluster/node.json", worker}, {"join-operation.json", map[string]string{"id": bundle.OperationID, "cluster_id": bundle.ClusterID, "node_id": bundle.NodeID, "package_hash": packageHash}}} {
		if err := saveClusterJSON(filepath.Join(staging, file.name), file.value, true); err != nil {
			return result, err
		}
	}
	if err := os.Rename(staging, root); err != nil {
		return result, err
	}
	parent, err := os.Open(filepath.Dir(root))
	if err != nil {
		return result, err
	}
	defer parent.Close()
	return result, parent.Sync()
}
