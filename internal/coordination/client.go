package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

type ClientConfig struct {
	TLS     TLSOptions
	Members []Member
	// ControlHeaders obtains owner authorization only for administrative calls.
	// These headers travel over a node-identity-verified mutual TLS connection.
	ControlHeaders   func(context.Context, string) (http.Header, error)
	Timeout          time.Duration
	MaxAttempts      int
	MaxResponseBytes int64
}

type Client struct {
	config  ClientConfig
	mu      sync.Mutex
	members map[string]Member
	leader  string
	clients map[string]*http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	if err := config.TLS.validate(); err != nil {
		return nil, err
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 6
	}
	if config.MaxResponseBytes <= 0 {
		config.MaxResponseBytes = 64 << 20
	}
	client := &Client{config: config, members: map[string]Member{}, clients: map[string]*http.Client{}}
	client.RememberMembers(config.Members)
	return client, nil
}

func (c *Client) RememberMembers(members []Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, member := range members {
		if member.NodeID != "" {
			c.members[member.NodeID] = member
		}
	}
}

func (c *Client) PeerID(address raft.ServerAddress) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, member := range c.members {
		if member.Address == string(address) {
			return id
		}
	}
	return ""
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range c.clients {
		client.CloseIdleConnections()
	}
}

func (c *Client) Status(ctx context.Context, member Member) (Status, error) {
	var status Status
	_, err := c.request(ctx, member, "status", nil, nil, &status)
	if err != nil {
		return Status{}, err
	}
	if status.NodeID != member.NodeID || status.ClusterID != c.config.TLS.ClusterID {
		return Status{}, fmt.Errorf("%w: peer status identity differs", ErrInvalid)
	}
	var members []Member
	for _, m := range status.Members {
		members = append(members, m)
	}
	c.RememberMembers(members)
	c.mu.Lock()
	c.leader = status.LeaderID
	c.mu.Unlock()
	return status, nil
}

func (c *Client) Probe(ctx context.Context, member Member) (Progress, error) {
	status, err := c.Status(ctx, member)
	if err != nil {
		return Progress{}, err
	}
	if !status.Healthy {
		return Progress{}, ErrUnavailable
	}
	return status.Progress(), nil
}

func (c *Client) ReadState(ctx context.Context) (State, error) {
	var state State
	err := c.route(ctx, "state", nil, &state)
	if err != nil {
		return State{}, err
	}
	if state.ClusterID != c.config.TLS.ClusterID {
		return State{}, fmt.Errorf("%w: cluster state identity differs", ErrInvalid)
	}
	var members []Member
	for _, member := range state.Members {
		members = append(members, member)
	}
	c.RememberMembers(members)
	return state, nil
}

func (c *Client) ApplyApp(ctx context.Context, request AppCommand) (Result, error) {
	if request.CallerNodeID != c.config.TLS.NodeID {
		return Result{}, ErrNotCoordinator
	}
	var result Result
	err := c.route(ctx, "app", request, &result)
	return result, err
}

func (c *Client) BeginWriter(ctx context.Context, request WriterRequest) (Result, error) {
	if request.CallerNodeID != c.config.TLS.NodeID {
		return Result{}, ErrNotCoordinator
	}
	var result Result
	err := c.route(ctx, "writer", request, &result)
	return result, err
}
func (c *Client) Transfer(ctx context.Context, request TransferRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "transfer", request, &result)
	return result, err
}
func (c *Client) SetAutoFailover(ctx context.Context, request PolicyRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "policy", request, &result)
	return result, err
}
func (c *Client) SetEligibility(ctx context.Context, request EligibilityRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "eligibility", request, &result)
	return result, err
}
func (c *Client) Join(ctx context.Context, request JoinRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "join", request, &result)
	return result, err
}
func (c *Client) Remove(ctx context.Context, request RemoveRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "remove", request, &result)
	return result, err
}

func (c *Client) UpdateMemberAddress(ctx context.Context, request MemberAddressRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "address", request, &result)
	return result, err
}

func (c *Client) route(ctx context.Context, action string, input, output any) error {
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	var headers http.Header
	if action != "app" && action != "writer" && action != "state" {
		if c.config.ControlHeaders == nil {
			return fmt.Errorf("%w: owner authorization is required", ErrInvalid)
		}
		var err error
		headers, err = c.config.ControlHeaders(ctx, action)
		if err != nil {
			return err
		}
	}
	visited := map[string]bool{}
	var last error = ErrUnavailable
	for attempt := 0; attempt < c.config.MaxAttempts; attempt++ {
		member, ok := c.nextMember(visited)
		if !ok {
			visited = map[string]bool{}
			member, ok = c.nextMember(visited)
			if !ok {
				return fmt.Errorf("%w: no peer HTTPS addresses are known", ErrUnavailable)
			}
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		visited[member.NodeID] = true
		failure, err := c.request(ctx, member, action, body, headers, output)
		if err == nil {
			c.mu.Lock()
			c.leader = member.NodeID
			c.mu.Unlock()
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if failure != nil {
			c.RememberMembers(failure.Members)
			c.mu.Lock()
			c.leader = failure.LeaderID
			c.mu.Unlock()
		}
		if !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrApplication) {
			return err
		}
	}
	return last
}

func (c *Client) nextMember(visited map[string]bool) (Member, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if member, ok := c.members[c.leader]; ok && !visited[c.leader] && member.APIAddress != "" {
		return member, true
	}
	var ids []string
	for id, member := range c.members {
		if !visited[id] && member.APIAddress != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return Member{}, false
	}
	return c.members[ids[0]], true
}

func (c *Client) peerClient(member Member) (*http.Client, string, error) {
	address, err := url.Parse(member.APIAddress)
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || (address.Path != "" && address.Path != "/") {
		return nil, "", fmt.Errorf("%w: peer API address must be an HTTPS origin", ErrInvalid)
	}
	key := member.NodeID + "\x00" + address.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	client := c.clients[key]
	if client == nil {
		config, err := c.config.TLS.ClientConfig(member.NodeID)
		if err != nil {
			return nil, "", err
		}
		transport := &http.Transport{TLSClientConfig: config, DialContext: (&net.Dialer{Timeout: c.config.Timeout}).DialContext, TLSHandshakeTimeout: c.config.Timeout, ResponseHeaderTimeout: c.config.Timeout, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4}
		client = &http.Client{Transport: transport, Timeout: c.config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		c.clients[key] = client
	}
	return client, strings.TrimSuffix(address.String(), "/"), nil
}

func (c *Client) request(ctx context.Context, member Member, action string, body []byte, headers http.Header, output any) (*rpcFailure, error) {
	client, origin, err := c.peerClient(member)
	if err != nil {
		return nil, err
	}
	method := http.MethodPost
	if action == "status" || action == "state" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, origin+RPCPath+action, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: peer %s request failed: %v", ErrUnavailable, member.NodeID, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.config.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read peer response: %v", ErrUnavailable, err)
	}
	if int64(len(data)) > c.config.MaxResponseBytes {
		return nil, fmt.Errorf("%w: peer response exceeds size limit", ErrInvalid)
	}
	if response.StatusCode != http.StatusOK {
		var failure rpcFailure
		if err := json.Unmarshal(data, &failure); err != nil {
			return nil, fmt.Errorf("%w: invalid peer error response", ErrInvalid)
		}
		return &failure, failure.err()
	}
	if err := json.Unmarshal(data, output); err != nil {
		return nil, fmt.Errorf("%w: invalid peer response", ErrInvalid)
	}
	return nil, nil
}
