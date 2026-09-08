package node

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

// BlobDir is where the hub's files land on this node.
func (s *Server) BlobDir() string { return filepath.Join(s.conf().StateDir, "blobs") }

// transferBlob serves one "put <name>" or "get <name>". Names are single
// path segments; the hub cannot reach outside the blob directory.
func (s *Server) transferBlob(ctx context.Context, stream *nodewire.Stream) {
	req := stream.Request()
	verb, name, _ := strings.Cut(req.Command, " ")
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		closeStream(stream, nodewire.ExitPrefix+"2")
		return
	}
	path := filepath.Join(s.BlobDir(), name)
	fail := func(code string) { closeStream(stream, nodewire.ExitPrefix+code) }
	switch verb {
	case "put":
		if err := os.MkdirAll(s.BlobDir(), 0o700); err != nil {
			fail("1")
			return
		}
		size, err := nodewire.ReadSize(stream)
		if err != nil {
			fail("1")
			return
		}
		file, err := os.CreateTemp(s.BlobDir(), "."+name+"-*")
		if err != nil {
			fail("1")
			return
		}
		temp := file.Name()
		n, err := io.Copy(file, io.LimitReader(stream, size))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil || n != size {
			os.Remove(temp)
			fail("1")
			return
		}
		if err := os.Rename(temp, path); err != nil {
			os.Remove(temp)
			fail("1")
			return
		}
		slog.Info(fmt.Sprintf("steve-node: received blob %s (%d bytes)", name, n), "blob", name)
		fail("0")
	case "get":
		file, err := os.Open(path)
		if err != nil {
			fail("1")
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			fail("1")
			return
		}
		if err := nodewire.WriteSize(stream, info.Size()); err != nil {
			fail("1")
			return
		}
		if _, err := io.Copy(stream, file); err != nil {
			fail("1")
			return
		}
		fail("0")
	default:
		fail("2")
	}
}

// fetch pulls a blob from a peer: "<addr> <token> <name>".
func (s *Server) fetch(ctx context.Context, stream *nodewire.Stream) {
	fields := strings.Fields(stream.Request().Command)
	fail := func(code string, err error) {
		if err != nil {
			slog.Error(fmt.Sprintf("steve-node: fetch: %v", err))
		}
		closeStream(stream, nodewire.ExitPrefix+code)
	}
	if len(fields) != 3 || fields[2] != filepath.Base(fields[2]) {
		fail("2", nil)
		return
	}
	addr, token, name := fields[0], fields[1], fields[2]
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: nodewire.HandshakeTimeout}
	socket, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		fail("1", err)
		return
	}
	defer socket.Close()
	if err := socket.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout)); err != nil {
		fail("1", err)
		return
	}
	if _, err := nodewire.Dial(socket, nodewire.Hello{Token: token, Hub: "peer:" + s.conf().Name}); err != nil {
		fail("1", err)
		return
	}
	// Clearing a deadline on a live socket cannot fail in a way the
	// transfer below would not report.
	_ = socket.SetDeadline(time.Time{})
	mux := nodewire.NewMux(socket, true)
	defer mux.Close()
	blob, err := mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamBlob, Command: "get " + name})
	if err != nil {
		fail("1", err)
		return
	}
	defer blob.Close()
	size, err := nodewire.ReadSize(blob)
	if err != nil {
		fail("1", err)
		return
	}
	if err := os.MkdirAll(s.BlobDir(), 0o700); err != nil {
		fail("1", err)
		return
	}
	file, err := os.CreateTemp(s.BlobDir(), "."+name+"-*")
	if err != nil {
		fail("1", err)
		return
	}
	temp := file.Name()
	n, err := io.Copy(file, io.LimitReader(blob, size))
	file.Close()
	if err != nil || n != size {
		os.Remove(temp)
		fail("1", fmt.Errorf("short read from peer: %d of %d", n, size))
		return
	}
	if err := os.Rename(temp, filepath.Join(s.BlobDir(), name)); err != nil {
		os.Remove(temp)
		fail("1", err)
		return
	}
	slog.Info(fmt.Sprintf("steve-node: fetched %s (%d bytes) from peer %s", name, n, addr), "blob", name, "peer", addr)
	closeStream(stream, nodewire.ExitPrefix+"0")
}
