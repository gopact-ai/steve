package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Completing a response is not the same as immediately closing a TCP socket
// with unread request bytes. Keep reading the bounded upload while the standard
// response writer sends its exact length or chunk terminator; wait for the
// recipient to close the exchange rather than resetting undelivered output.
func completeEarlyResponse(t *testing.T, body string, chunked bool, calls *atomic.Int32) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	resume := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	address := reverseRawServer(t, func(connection net.Conn, request *http.Request) {
		calls.Add(1)
		var first [1]byte
		if _, err := io.ReadFull(request.Body, first[:]); err != nil {
			t.Error(err)
			return
		}
		readDone := make(chan struct{})
		go func() {
			io.CopyN(io.Discard, request.Body, reverseRequestLimit)
			close(readDone)
		}()
		defer func() {
			connection.SetReadDeadline(time.Now())
			select {
			case <-readDone:
			case <-time.After(time.Second):
				t.Error("response fixture retained its request reader")
			}
		}()
		response := &http.Response{StatusCode: http.StatusConflict, ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": {"text/plain"}}, ContentLength: int64(len(body)),
			Body: io.NopCloser(&stagedReverseBody{ctx: ctx, body: strings.NewReader(body), resume: resume})}
		if chunked {
			response.ContentLength, response.TransferEncoding = -1, []string{"chunked"}
		}
		if err := response.Write(connection); err != nil {
			t.Errorf("complete early response: %v", err)
			return
		}
		select {
		case <-readDone:
		case <-ctx.Done():
			t.Error("response recipient did not close the completed exchange")
		}
	})
	t.Cleanup(func() { release(); cancel() })
	return address, release
}

type stagedReverseBody struct {
	ctx    context.Context
	body   *strings.Reader
	resume <-chan struct{}
	first  bool
}

func (r *stagedReverseBody) Read(p []byte) (int, error) {
	if r.first {
		select {
		case <-r.resume:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	r.first = true
	return r.body.Read(p[:min(len(p), 8192)])
}

// Even if the response headers were parsed before the upload's write error is
// handled, a real stream close orders all received response bytes before EOF.
// Exercise writer-error-first selection without substituting an HTTP response.
func TestReverseHTTPParsedResponseSurvivesWriterErrorSelection(t *testing.T) {
	for _, chunkedResponse := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked-response-%v", chunkedResponse), func(t *testing.T) {
			want := strings.Repeat("response-data\n", 100000)
			firstWritten, sendRest := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(sendRest) }) }
			address := reverseRawServer(t, func(connection net.Conn, request *http.Request) {
				var first [1]byte
				if _, err := io.ReadFull(request.Body, first[:]); err != nil {
					t.Error(err)
					return
				}
				response := &http.Response{StatusCode: http.StatusConflict, ProtoMajor: 1, ProtoMinor: 1,
					Header: http.Header{"Content-Type": {"text/plain"}}, ContentLength: int64(len(want)),
					Body: io.NopCloser(&notifyingReverseBody{body: strings.NewReader(want), first: firstWritten, resume: sendRest})}
				if chunkedResponse {
					response.ContentLength, response.TransferEncoding = -1, []string{"chunked"}
				}
				if err := response.Write(connection); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(release)
			dial := reverseHTTPDialer(t, address)
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			base, err := dial(ctx)
			if err != nil {
				t.Fatal(err)
			}
			connection := &deferredUploadWrite{reverseHTTPConnection: base, closed: make(chan struct{})}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hub/mcp", &reverseBody{strings.NewReader(strings.Repeat("x", 128<<10))})
			if err != nil {
				t.Fatal(err)
			}
			request.ContentLength = 1 << 20
			x := newReverseExchange(request, connection, func() error { return nil })
			defer x.close()
			var reply reverseHTTPReply
			select {
			case reply = <-x.reply:
				if reply.err != nil {
					t.Fatal(reply.err)
				}
			case <-ctx.Done():
				t.Fatal("response headers were not parsed")
			}
			select {
			case <-firstWritten:
			case <-ctx.Done():
				t.Fatal("response body did not start")
			}
			release()
			select {
			case <-connection.Done():
			case <-ctx.Done():
				t.Fatal("complete response did not reach the stream")
			}
			select {
			case <-x.writeDone:
			case <-ctx.Done():
				t.Fatal("request write did not report the real stream close")
			}
			writeErr := <-x.written
			if writeErr == nil {
				t.Fatal("request writer did not fail")
			}
			x.written <- writeErr
			forwarded := make(chan struct{})
			go func() {
				defer close(forwarded)
				select {
				case <-connection.closed:
					x.reply <- reply
				case <-ctx.Done():
				}
			}()
			response, err := x.response(ctx)
			<-forwarded
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(got) != want {
				t.Fatalf("parsed response lost after writer error %v: bytes=%d want=%d error=%v", writeErr, len(got), len(want), err)
			}
		})
	}
}

type notifyingReverseBody struct {
	body   *strings.Reader
	first  chan struct{}
	resume <-chan struct{}
	seen   bool
}

func (b *notifyingReverseBody) Read(p []byte) (int, error) {
	if b.seen {
		select {
		case <-b.resume:
		case <-time.After(3 * time.Second):
			return 0, fmt.Errorf("response remainder was not released")
		}
	} else {
		b.seen = true
		close(b.first)
	}
	return b.body.Read(p[:min(len(p), 8192)])
}

type deferredUploadWrite struct {
	reverseHTTPConnection
	writes int
	closed chan struct{}
	once   sync.Once
}

func (c *deferredUploadWrite) Write(p []byte) (int, error) {
	c.writes++
	if c.writes == 2 {
		// Send one genuine request byte, then hold its remainder until the peer
		// has finished the full response and closed the real stream.
		n, err := c.reverseHTTPConnection.Write(p[:1])
		if err != nil {
			return n, err
		}
		select {
		case <-c.Done():
		case <-time.After(3 * time.Second):
			return n, fmt.Errorf("upstream did not finish the response")
		}
		rest, err := c.reverseHTTPConnection.Write(p[1:])
		return n + rest, err
	}
	return c.reverseHTTPConnection.Write(p)
}

func (c *deferredUploadWrite) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.reverseHTTPConnection.Close()
}
