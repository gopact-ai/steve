package nodewire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// pair returns two muxes over a real socket pair, dialer first.
func pair(t *testing.T) (*Mux, *Mux) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	a, b := NewMux(client, true), NewMux(server, false)
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestStreamRoundTrip(t *testing.T) {
	hub, node := pair(t)
	go func() {
		s, err := node.Accept(context.Background())
		if err != nil {
			return
		}
		if s.Request().Kind != StreamACP || s.Request().Harness != "codex" {
			t.Errorf("open request = %+v", s.Request())
		}
		_, _ = io.Copy(s, s)
		_ = s.Close()
	}()
	s, err := hub.Open(OpenRequest{Kind: StreamACP, Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(s, "hello node\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "hello node\n" {
		t.Fatalf("echo = %q", got)
	}
}

// TestConcurrentStreamsStayIndependent is the property the whole design rests
// on: one connection per node, many sessions, no crosstalk.
func TestConcurrentStreamsStayIndependent(t *testing.T) {
	hub, node := pair(t)
	go func() {
		for {
			s, err := node.Accept(context.Background())
			if err != nil {
				return
			}
			go func(s *Stream) {
				_, _ = io.Copy(s, s)
				_ = s.Close()
			}(s)
		}
	}()

	const streams, rounds = 8, 20
	var wg sync.WaitGroup
	for i := range streams {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := hub.Open(OpenRequest{Kind: StreamACP})
			if err != nil {
				t.Errorf("open %d: %v", i, err)
				return
			}
			defer s.Close()
			want := strings.Repeat(string(rune('a'+i)), 512)
			for range rounds {
				if _, err := io.WriteString(s, want); err != nil {
					t.Errorf("write %d: %v", i, err)
					return
				}
				got := make([]byte, len(want))
				if _, err := io.ReadFull(s, got); err != nil {
					t.Errorf("read %d: %v", i, err)
					return
				}
				if string(got) != want {
					t.Errorf("stream %d crosstalk: got %q", i, string(got)[:16])
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestLargeWriteIsSplitAndReassembled covers payloads past one frame — an
// agent that emits a big diff must not wedge the connection.
func TestLargeWriteIsSplitAndReassembled(t *testing.T) {
	hub, node := pair(t)
	go func() {
		s, err := node.Accept(context.Background())
		if err != nil {
			return
		}
		_, _ = io.Copy(s, s)
		_ = s.Close()
	}()
	s, err := hub.Open(OpenRequest{Kind: StreamACP})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("x"), MaxPayload+1024)
	go func() { _, _ = s.Write(want) }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("large payload did not survive framing")
	}
}

// TestConnectionDropWakesBlockedReaders is what a node going offline looks
// like: every session on it must fail, not hang.
func TestConnectionDropWakesBlockedReaders(t *testing.T) {
	hub, node := pair(t)
	go func() {
		if _, err := node.Accept(context.Background()); err != nil {
			return
		}
	}()
	s, err := hub.Open(OpenRequest{Kind: StreamACP})
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 16))
		read <- err
	}()
	node.Close()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("read returned nil after the connection dropped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read stayed blocked after the connection dropped")
	}
	select {
	case <-hub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("hub mux never noticed the drop")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	hub, node := pair(t)
	go func() { _, _ = node.Accept(context.Background()) }()
	s, err := hub.Open(OpenRequest{Kind: StreamACP})
	if err != nil {
		t.Fatal(err)
	}
	// acphost closes stdin and stdout separately; both land here.
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestHandshakeAcceptsThenRefuses(t *testing.T) {
	advert := Advert{
		Node: "host-3", OS: "linux", Arch: "amd64",
		Harnesses:    []Harness{{ID: "codex", Command: "npx", Models: []string{"gpt-5"}}},
		Capabilities: []string{"gpu"},
	}

	t.Run("good token", func(t *testing.T) {
		hubSide, nodeSide := net.Pipe()
		go func() {
			if _, err := Accept(nodeSide, "s3cret", advert); err != nil {
				t.Errorf("accept: %v", err)
			}
		}()
		got, err := Dial(hubSide, Hello{Token: "s3cret", Hub: "hub-1"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Node != "host-3" || len(got.Harnesses) != 1 || got.Harnesses[0].Models[0] != "gpt-5" {
			t.Fatalf("advert did not survive: %+v", got)
		}
	})

	t.Run("bad token", func(t *testing.T) {
		hubSide, nodeSide := net.Pipe()
		go func() { _, _ = Accept(nodeSide, "s3cret", advert) }()
		if _, err := Dial(hubSide, Hello{Token: "wrong"}); !errors.Is(err, ErrBadToken) {
			t.Fatalf("err = %v, want ErrBadToken", err)
		}
	})
}
