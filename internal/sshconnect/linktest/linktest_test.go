package linktest_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/sshconnect"
	"github.com/gopact-ai/steve/internal/sshconnect/linktest"
)

// A reserved address is bound from the moment it is handed out until the
// test ends. Each session that listens there serves on the same socket,
// so nothing else can take the port before the far end or between two
// sessions.
func TestAReservedAddressStaysBoundAcrossSessions(t *testing.T) {
	sessions := &linktest.Launcher{}
	address := sessions.Reserve(t)
	if taken, err := net.Listen("tcp", address); err == nil {
		taken.Close()
		t.Fatal("a reserved port could be bound before the far end served on it")
	}
	target := echo(t)
	link := sshconnect.OpenLink(t.Context(), sshconnect.LinkSpec{Alias: "dev", Inbound: []sshconnect.PortForward{{Listen: address, Target: target}}}, sshconnect.LinkOptions{Launch: sessions, Backoff: func(int) time.Duration { return 10 * time.Millisecond }})
	t.Cleanup(link.Close)
	connected(t, link)
	if err := roundTrip(address, "first"); err != nil {
		t.Fatalf("the far end does not serve on the reserved port: %v", err)
	}
	sessions.Drop(errors.New("connection reset"))
	<-sessions.Ended(0)
	if taken, err := net.Listen("tcp", address); err == nil {
		taken.Close()
		t.Fatal("the reserved port was released between sessions")
	}
	deadline := time.Now().Add(5 * time.Second)
	for sessions.Count() < 2 || !link.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatalf("the link did not come back: %+v", link.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := roundTrip(address, "second"); err != nil {
		t.Fatalf("the next session does not serve on the reserved port: %v", err)
	}
}

func connected(t *testing.T, link *sshconnect.Link) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := link.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
}

func echo(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(connection, connection)
				connection.Close()
			}()
		}
	}()
	return listener.Addr().String()
}

func roundTrip(address, line string) error {
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(connection, line+"\n"); err != nil {
		return err
	}
	got, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != line {
		return errors.New("echo returned " + got)
	}
	return nil
}
