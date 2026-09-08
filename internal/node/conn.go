package node

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// conn is one live node: the multiplexed connection plus what the node said
// it can run when it answered.
type conn struct {
	name       string
	mux        *nodewire.Mux
	generation int64
	released   atomic.Bool

	advMu  sync.RWMutex
	advert nodewire.Advert
	// lastBindings are the MCP launchers a node handed back per attempt
	// at admission, held until the session open takes them.
	bindingsMu   sync.Mutex
	lastBindings map[string][]ability.Binding

	closeOnce sync.Once
	// reverseOnce guards the reverse MCP server: started at dial time
	// when the dialer is known, or later when it is wired — the hub dials
	// its machines before its messaging server exists.
	reverseOnce sync.Once
}

// startReverse serves the node's reverse MCP streams, once.
func (c *conn) startReverse(mcpDial func(context.Context) (net.Conn, error)) {
	if mcpDial == nil {
		return
	}
	c.reverseOnce.Do(func() { go c.serveReverse(mcpDial) })
}

func (c *conn) getAdvert() nodewire.Advert {
	c.advMu.RLock()
	defer c.advMu.RUnlock()
	return c.advert
}

func (c *conn) setAdvert(adv nodewire.Advert) {
	c.advMu.Lock()
	defer c.advMu.Unlock()
	c.advert = adv
}

func dial(ctx context.Context, name, hub string, cfg Config, mcpDial func(context.Context) (net.Conn, error)) (*conn, error) {
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	var socket net.Conn
	var err error
	if cfg.DialContext != nil {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		socket, err = cfg.DialContext(dialCtx, name)
		cancel()
	} else {
		socket, err = dialer.DialContext(ctx, "tcp", cfg.Addr)
	}
	if err != nil {
		return nil, err
	}
	// The handshake is the one exchange with a hard deadline: past it the
	// connection is long-lived and its streams carry their own timeouts.
	if err := socket.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout)); err != nil {
		socket.Close()
		return nil, err
	}
	advert, err := nodewire.Dial(socket, nodewire.Hello{Token: cfg.Token, Hub: hub, Features: nodewire.Features()})
	if err != nil {
		socket.Close()
		return nil, err
	}
	// Clearing a deadline on a live socket cannot fail in a way the mux's
	// first read would not report.
	_ = socket.SetDeadline(time.Time{})

	c := &conn{name: name, mux: nodewire.NewMux(socket, true), advert: advert}
	c.startReverse(mcpDial)
	log.Printf("node: %s up — %s/%s, harnesses=%d, caps=%v",
		name, advert.OS, advert.Arch, len(advert.Harnesses), advert.Capabilities)
	return c, nil
}

func (c *conn) alive() bool {
	select {
	case <-c.mux.Done():
		return false
	default:
		return true
	}
}

func (c *conn) close() {
	c.closeOnce.Do(func() {
		c.released.Store(true)
		// This connection is being given up; how its socket went down
		// is not news to anyone.
		if nodewire.HasFeature(c.getAdvert().Features, nodewire.FeatureJournal) {
			_ = c.mux.CloseGracefully()
		} else {
			_ = c.mux.Close()
		}
	})
}

// serveReverse forwards the node's MCP streams to the hub's loopback
// messaging server. The remote agent still only ever talks to a loopback
// address on its own machine — the node listens there and tunnels here — so
// "loopback only, one bearer token per session" survives the move to another
// host instead of being traded away for an exposed port.
func (c *conn) serveReverse(mcpDial func(context.Context) (net.Conn, error)) {
	for {
		stream, err := c.mux.Accept(context.Background())
		if err != nil {
			return
		}
		if stream.Request().Kind != nodewire.StreamMCP {
			// The hub opens ACP streams; it never accepts them. The refusal
			// is the close itself.
			_ = stream.Close()
			continue
		}
		go func(stream *nodewire.Stream) {
			defer stream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			upstream, err := mcpDial(ctx)
			cancel()
			if err != nil {
				log.Printf("node: %s reverse MCP dial: %v", c.name, err)
				return
			}
			defer upstream.Close()
			// A proxy pump ends when either side closes; which side and
			// why belongs to the two ends, not to the pump, so the copies
			// and the closes that follow report nothing here.
			go func() { <-stream.Done(); _ = upstream.Close() }()
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(upstream, stream)
				// Half-close so the server sees the end of the request.
				if tcp, ok := upstream.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
				close(done)
			}()
			_, _ = io.Copy(stream, upstream)
			_ = stream.Close()
			_ = upstream.Close()
			<-done
		}(stream)
	}
}
