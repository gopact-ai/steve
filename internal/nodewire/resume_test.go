package nodewire

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestResumeMetadataAckAndPayloadOrdering(t *testing.T) {
	a, b := net.Pipe()
	hub, node := NewMux(a, true), NewMux(b, false)
	defer hub.Close()
	defer node.Close()
	want := OpenRequest{Kind: StreamACP, Harness: "h", Stream: "process-1", Resume: true, AfterOut: 3, AfterIn: 7}
	go func() {
		s, err := node.Accept(context.Background())
		if err != nil {
			return
		}
		if s.Request() != want {
			t.Errorf("request = %+v", s.Request())
		}
		_ = (ResumeAck{HaveIn: 5}).Write(s)
		_ = s.AckInput(6)
		_, _ = io.WriteString(s, "output 4\noutput 5\n")
		_ = s.CloseWithReason("exit 0")
	}()
	s, err := hub.Open(want)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(s)
	ack, err := ReadResumeAck(r)
	if err != nil || ack.HaveIn != 5 {
		t.Fatal(ack, err)
	}
	body, err := io.ReadAll(r)
	if string(body) != "output 4\noutput 5\n" || err == nil || err.Error() != "exit 0" {
		t.Fatal(string(body), err)
	}
	if have, _ := s.InputAck(); have != 6 {
		t.Fatal(have)
	}
}

func TestGoodbyeDiffersFromSocketLoss(t *testing.T) {
	for _, clean := range []bool{false, true} {
		a, b := net.Pipe()
		hub, node := NewMux(a, true), NewMux(b, false)
		if clean {
			_ = hub.CloseGracefully()
		} else {
			_ = hub.Close()
		}
		select {
		case <-node.Done():
		case <-time.After(time.Second):
			t.Fatal("peer stayed connected")
		}
		if node.Graceful() != clean {
			t.Fatalf("clean=%v, got %v", clean, node.Graceful())
		}
		_ = node.Close()
	}
}

func TestStalledStreamCannotBlockOtherStreamsOrConnectionLoss(t *testing.T) {
	hub, node := pair(t)
	stalled, err := hub.Open(OpenRequest{Kind: StreamACP})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := node.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range streamBufferBytes/MaxPayload + 1 {
		if _, err := stalled.Write(bytes.Repeat([]byte("x"), MaxPayload)); err != nil {
			break
		}
	}
	select {
	case <-remote.Done():
	case <-time.After(time.Second):
		t.Fatal("stalled stream did not close")
	}
	other, err := hub.Open(OpenRequest{Kind: StreamACP})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := node.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.WriteString(peer, "independent\n") }()
	line, err := bufio.NewReader(other).ReadString('\n')
	if err != nil || line != "independent\n" {
		t.Fatal(line, err)
	}
	_ = node.Close()
	_, err = other.Read(make([]byte, 1))
	if !errors.Is(err, ErrMuxClosed) {
		t.Fatal(err)
	}
}
