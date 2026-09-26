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
	ControlHeaders func(context.Context, string) (http.Header, error)
	// SelfHost is the host this node's own HTTPS listener is bound to. Calls
	// aimed at this node go there instead of the advertised address; empty or
	// unspecified means loopback.
	SelfHost string
	// Routes is where this node connects to nodes it cannot reach at what
	// they advertise; nil means every node is dialed at its advertised
	// address.
	Routes  *RouteTable
	Timeout time.Duration
	// RetryWindow is how long a routed call keeps trying members while none
	// takes it: no member is the consensus leader, one is unavailable or in
	// a leadership transfer, or its replica stopped. It has to outlast a
	// leader election; zero means DefaultRetryWindow. A call sent before the
	// window ends may still take up to Timeout.
	RetryWindow      time.Duration
	MaxResponseBytes int64
}

// DefaultRetryWindow outlasts one leader election under Raft's default
// timings, which Service uses unless Config.RaftConfig changes them: with a
// one-second heartbeat and election timeout, followers notice a lost leader
// within two seconds and an election round ends within two more. An election
// that needs further rounds after split votes can outlast it; the call then
// fails as unavailable and can be retried.
const DefaultRetryWindow = 5 * time.Second

// Once every known member has refused a routed call, it pauses before the
// next round: firstRetryPause at first, doubling up to maxRetryPause.
const (
	firstRetryPause = 50 * time.Millisecond
	maxRetryPause   = 500 * time.Millisecond
)

type Client struct {
	config  ClientConfig
	mu      sync.Mutex
	members map[string]Member
	leader  string
	clients map[string]pooledClient
}

// pooledClient is the HTTP client this node keeps for one other node,
// together with the address and route it was built to reach. When either
// changes the client is rebuilt: its idle connections point at the old way
// there (a torn-down tunnel, a previous address) and must not be reused.
type pooledClient struct {
	client  *http.Client
	address string
	route   string
}

func NewClient(config ClientConfig) (*Client, error) {
	if err := config.TLS.validate(); err != nil {
		return nil, err
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	if config.RetryWindow <= 0 {
		config.RetryWindow = DefaultRetryWindow
	}
	if config.MaxResponseBytes <= 0 {
		config.MaxResponseBytes = 64 << 20
	}
	client := &Client{config: config, members: map[string]Member{}, clients: map[string]pooledClient{}}
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
	for _, pooled := range c.clients {
		pooled.client.CloseIdleConnections()
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

// ReadIndex asks the consensus leader for a read index; see
// Service.ReadIndex.
func (c *Client) ReadIndex(ctx context.Context) (uint64, error) {
	var reply readIndexReply
	if err := c.route(ctx, "readindex", nil, &reply); err != nil {
		return 0, err
	}
	return reply.Index, nil
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
func (c *Client) Rename(ctx context.Context, request RenameRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "rename", request, &result)
	return result, err
}
func (c *Client) SetVoting(ctx context.Context, request VotingRequest) (Result, error) {
	var result Result
	err := c.route(ctx, "voting", request, &result)
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
	if action != "app" && action != "writer" && action != "state" && action != "readindex" {
		if c.config.ControlHeaders == nil {
			return fmt.Errorf("%w: owner authorization is required", ErrInvalid)
		}
		var err error
		headers, err = c.config.ControlHeaders(ctx, action)
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(c.config.RetryWindow)
	visited := map[string]bool{}
	pause := firstRetryPause
	var last error // the latest retryable failure
	for {
		member, ok := c.nextMember(visited)
		if !ok {
			// Every known member refused the call in this round.
			visited = map[string]bool{}
			member, ok = c.nextMember(visited)
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return fmt.Errorf("%w: no peer HTTPS addresses are known", ErrUnavailable)
			}
			timer := time.NewTimer(min(pause, max(time.Until(deadline), 0)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			pause = min(2*pause, maxRetryPause)
		}
		// No call starts once the window has ended, including after a pause
		// the window cut short.
		if last != nil && !time.Now().Before(deadline) {
			return fmt.Errorf("%w: no member took %s within %s: %w", ErrUnavailable, action, c.config.RetryWindow, last)
		}
		visited[member.NodeID] = true
		failure, err := c.request(ctx, member, action, body, headers, output)
		if err == nil {
			c.mu.Lock()
			c.leader = member.NodeID
			c.mu.Unlock()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if failure != nil {
			c.RememberMembers(failure.Members)
			c.mu.Lock()
			c.leader = failure.LeaderID
			c.mu.Unlock()
		}
		// A member whose application replica failed stops its Raft node, so
		// another member takes over, as after a lost leader. A retried
		// command keeps its ID, which the leader deduplicates.
		if !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrApplication) {
			return err
		}
		last = err
	}
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
	route, _ := c.config.Routes.Lookup(member.NodeID)
	c.mu.Lock()
	defer c.mu.Unlock()
	pooled, ok := c.clients[member.NodeID]
	if ok && (pooled.address != address.String() || pooled.route != route.API) {
		pooled.client.CloseIdleConnections()
		delete(c.clients, member.NodeID)
		ok = false
	}
	if !ok {
		config, err := c.config.TLS.ClientConfig(member.NodeID)
		if err != nil {
			return nil, "", err
		}
		dial := (&net.Dialer{Timeout: c.config.Timeout}).DialContext
		switch {
		case member.NodeID == c.config.TLS.NodeID:
			dial = SelfDial(c.config.SelfHost, dial)
		case route.API != "":
			dial = c.config.Routes.APIDial(member.NodeID, dial)
		}
		transport := &http.Transport{TLSClientConfig: config, DialContext: dial, TLSHandshakeTimeout: c.config.Timeout, ResponseHeaderTimeout: c.config.Timeout, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4}
		client := &http.Client{Transport: transport, Timeout: c.config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		pooled = pooledClient{client: client, address: address.String(), route: route.API}
		c.clients[member.NodeID] = pooled
	}
	return pooled.client, strings.TrimSuffix(address.String(), "/"), nil
}

func (c *Client) request(ctx context.Context, member Member, action string, body []byte, headers http.Header, output any) (*rpcFailure, error) {
	client, origin, err := c.peerClient(member)
	if err != nil {
		return nil, err
	}
	method := http.MethodPost
	if action == "status" || action == "state" || action == "readindex" {
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
	// A reply that is not the peer's answer to the request says nothing about
	// the request, so none of these is reported as invalid input.
	if int64(len(data)) > c.config.MaxResponseBytes {
		return nil, fmt.Errorf("coordination: peer %s response exceeds %d bytes", member.NodeID, c.config.MaxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		var failure rpcFailure
		if err := json.Unmarshal(data, &failure); err != nil || failure.Code == "" {
			// Something other than the coordination handler answered, such
			// as a proxy or a server that is stopping; a 5xx page from it
			// is a transport failure.
			if response.StatusCode >= http.StatusInternalServerError {
				return nil, fmt.Errorf("%w: peer %s answered HTTP %d without a coordination error", ErrUnavailable, member.NodeID, response.StatusCode)
			}
			return nil, fmt.Errorf("coordination: peer %s answered HTTP %d without a coordination error", member.NodeID, response.StatusCode)
		}
		return &failure, failure.err()
	}
	if err := json.Unmarshal(data, output); err != nil {
		return nil, fmt.Errorf("coordination: read peer %s response: %v", member.NodeID, err)
	}
	return nil, nil
}

// DialFunc connects to a network address; it matches net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// SelfDial connects to the local listener behind an address this node
// advertises for itself. A machine cannot always route to its own address
// (a VPN tunnel address is reachable from every other machine but often not
// from the laptop itself), and the process answering there is this one
// anyway. bindHost is where the listener really is; a wildcard or empty
// host means loopback. Mutual TLS still verifies the node identity on the
// connection, so the check keeps proving the port serves this node.
func SelfDial(bindHost string, dial DialFunc) DialFunc {
	if ip := net.ParseIP(bindHost); bindHost == "" || ip != nil && ip.IsUnspecified() {
		bindHost = "127.0.0.1"
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, port, err := net.SplitHostPort(address); err == nil {
			address = net.JoinHostPort(bindHost, port)
		}
		return dial(ctx, network, address)
	}
}
