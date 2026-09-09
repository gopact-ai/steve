// Package tui renders the read model in a terminal.
//
// It is a client of the same HTTP surface the dashboard uses, not a
// privileged reader of the stores. That is deliberate: two renderers over one
// read model cannot disagree about what is happening, and a bug in what the
// operator sees is then a bug in one place.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
	"golang.org/x/term"
)

// Config points the renderer at a running hub.
type Config struct {
	URL   string
	Token string
	// Refresh is the floor: the screen also redraws whenever the event
	// stream says something moved, so this only covers a silent stream.
	Refresh time.Duration
}

type Model struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	snap     readmodel.Snapshot
	feed     []readmodel.Event
	live     bool
	oneShot  bool
	lastErr  string
	width    int
	height   int
	viewMode int
}

const (
	viewOverview = iota
	viewPlans
	viewFeed
	viewLedger
	viewCount
)

const feedKept = 200

func New(cfg Config) *Model {
	if cfg.Refresh <= 0 {
		cfg.Refresh = 5 * time.Second
	}
	return &Model{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
		width:  100, height: 40,
	}
}

// Run draws until the context ends or the user quits.
func (m *Model) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	restore, err := m.enterFullScreen()
	if err != nil {
		return err
	}
	defer restore()

	m.measure()
	redraw := make(chan struct{}, 1)
	poke := func() {
		select {
		case redraw <- struct{}{}:
		default:
		}
	}

	go m.watch(ctx, poke)
	go m.keys(ctx, cancel, poke)

	ticker := time.NewTicker(m.cfg.Refresh)
	defer ticker.Stop()
	m.refresh(ctx)
	m.draw()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.measure()
			m.refresh(ctx)
			m.draw()
		case <-redraw:
			m.measure()
			m.refresh(ctx)
			m.draw()
		}
	}
}

func (m *Model) enterFullScreen() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, fmt.Errorf("steve top needs a terminal; pipe /state into jq instead")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	return func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		// The screen is already handed back; a raw mode that will not
		// undo itself is the user's to fix, so say so where they can see it.
		if err := term.Restore(fd, state); err != nil {
			fmt.Fprintf(os.Stderr, "steve top: restore terminal: %v (run `reset`)\n", err)
		}
	}, nil
}

func (m *Model) measure() {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		m.mu.Lock()
		m.width, m.height = w, h
		m.mu.Unlock()
	}
}

// keys handles the few controls worth having: quit, cycle view, force refresh.
func (m *Model) keys(ctx context.Context, cancel context.CancelFunc, poke func()) {
	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || ctx.Err() != nil {
			return
		}
		switch strings.ToLower(string(buf[:n])) {
		case "q", "\x03": // q or ctrl-c
			cancel()
			return
		case "\t", "v":
			m.mu.Lock()
			m.viewMode = (m.viewMode + 1) % viewCount
			m.mu.Unlock()
			poke()
		case "r":
			poke()
		}
	}
}

// watch follows the change stream so the screen reacts when something moves
// rather than on the next tick.
func (m *Model) watch(ctx context.Context, poke func()) {
	for ctx.Err() == nil {
		if err := m.stream(ctx, poke); err != nil && ctx.Err() == nil {
			m.mu.Lock()
			m.live, m.lastErr = false, err.Error()
			m.mu.Unlock()
			poke()
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (m *Model) stream(ctx context.Context, poke func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.URL+"/events", nil)
	if err != nil {
		return err
	}
	m.authorize(req)
	// The stream is long-lived; the client's own timeout must not cut it.
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", res.Status)
	}
	m.mu.Lock()
	m.live, m.lastErr = true, ""
	m.mu.Unlock()
	poke()

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev readmodel.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		m.mu.Lock()
		m.feed = append(m.feed, ev)
		if len(m.feed) > feedKept {
			m.feed = m.feed[len(m.feed)-feedKept:]
		}
		m.mu.Unlock()
		poke()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (m *Model) refresh(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.URL+"/state", nil)
	if err != nil {
		return
	}
	m.authorize(req)
	res, err := m.client.Do(req)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		m.mu.Lock()
		m.lastErr = "state: " + res.Status
		m.mu.Unlock()
		return
	}
	var snap readmodel.Snapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.snap, m.lastErr = snap, ""
	m.mu.Unlock()
}

func (m *Model) authorize(req *http.Request) {
	if m.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}
}

// Snapshot exposes the current state for tests and for a one-shot render.
func (m *Model) Snapshot() readmodel.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

// Once fetches and renders a single frame, for a non-terminal caller.
// It reports the frame as a snapshot rather than as a dropped stream: it
// never opened one, and "offline" would be describing this call rather than
// the gateway.
func (m *Model) Once(ctx context.Context) string {
	m.refresh(ctx)
	m.mu.Lock()
	m.oneShot = true
	m.mu.Unlock()
	return m.render()
}

func (m *Model) draw() {
	fmt.Print("\x1b[H\x1b[2J")
	fmt.Print(strings.ReplaceAll(m.render(), "\n", "\r\n"))
}

func sortedNodes(nodes []readmodel.Node) []readmodel.Node {
	out := append([]readmodel.Node{}, nodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
