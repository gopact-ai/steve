package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

type reverseHTTPConnection interface {
	net.Conn
	Done() <-chan struct{}
}

type reverseReadAbortKey struct{}
type reverseReadAbort func() error

// This listener owns the TCP connection. Supplying the interruption capability
// here avoids changing an existing read deadline just to probe HTTP wrappers.
func reverseConnectionContext(ctx context.Context, connection net.Conn) context.Context {
	abort := reverseReadAbort(func() error {
		if err := connection.SetReadDeadline(time.Now()); err != nil {
			// An unsupported or failed interrupt must still release its reader.
			// This only runs when the response is finished or explicitly cancelled.
			return errors.Join(err, connection.Close())
		}
		return nil
	})
	return context.WithValue(ctx, reverseReadAbortKey{}, abort)
}

func prepareReverseHTTP(w http.ResponseWriter, r *http.Request) error {
	abort, ok := r.Context().Value(reverseReadAbortKey{}).(reverseReadAbort)
	if !ok || abort == nil {
		return errors.New("reverse HTTP connection cannot interrupt its upload")
	}
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		return err
	}
	// Finishing an early response interrupts any unread upload. This connection
	// cannot carry another request afterwards; the server must not drain it first.
	w.Header().Set("Connection", "close")
	if r.ContentLength > reverseRequestLimit {
		return &http.MaxBytesError{Limit: reverseRequestLimit}
	}
	return nil
}

func releaseReverseInput(request *http.Request) {
	if abort, ok := request.Context().Value(reverseReadAbortKey{}).(reverseReadAbort); ok && abort != nil {
		abort()
	}
	if request.Body != nil {
		request.Body.Close()
	}
}

type reverseHTTP struct {
	dial func(context.Context) (reverseHTTPConnection, error)
}

func newReverseHTTP(dial func(context.Context) (reverseHTTPConnection, error)) *reverseHTTP {
	return &reverseHTTP{dial: dial}
}

// RoundTrip exchanges one request over one stream, without replay. A request
// writer's EOF cannot outrank a final response already received on that stream.
// Standard HTTP readers still detect incomplete response headers and bodies.
func (t *reverseHTTP) RoundTrip(request *http.Request) (*http.Response, error) {
	abort, ok := request.Context().Value(reverseReadAbortKey{}).(reverseReadAbort)
	if !ok || abort == nil {
		return nil, errors.New("reverse HTTP request has no upload interruption capability")
	}
	if request.ContentLength > reverseRequestLimit {
		releaseReverseInput(request)
		return nil, &http.MaxBytesError{Limit: reverseRequestLimit}
	}
	if err := request.Context().Err(); err != nil {
		releaseReverseInput(request)
		return nil, err
	}
	connection, err := t.dial(request.Context())
	if err != nil {
		releaseReverseInput(request)
		return nil, err
	}
	x := newReverseExchange(request, connection, abort)
	stop := context.AfterFunc(request.Context(), x.close)
	finish := func() { stop(); x.close() }
	response, err := x.response(request.Context())
	if err != nil {
		finish()
		return nil, err
	}
	if x.upload != nil {
		// This only stops further upload reads. It must not interrupt the actual
		// client read (which cancels its HTTP context) until the response ends.
		x.upload.Close()
	}
	response.Body = &reverseResponseBody{ReadCloser: response.Body, finish: finish, upload: x.upload}
	return response, nil
}

type reverseHTTPReply struct {
	response *http.Response
	err      error
}

type reverseExchange struct {
	connection reverseHTTPConnection
	upload     *reverseUpload
	abort      reverseReadAbort
	written    chan error
	writeDone  chan struct{}
	reply      chan reverseHTTPReply
	readDone   chan struct{}
	ended      chan struct{}
	watchDone  chan struct{}
	closeOnce  sync.Once
}

func newReverseExchange(request *http.Request, connection reverseHTTPConnection, abort reverseReadAbort) *reverseExchange {
	x := &reverseExchange{connection: connection, abort: abort, written: make(chan error, 1), writeDone: make(chan struct{}), reply: make(chan reverseHTTPReply, 1), readDone: make(chan struct{}), ended: make(chan struct{}), watchDone: make(chan struct{})}
	out := request.Clone(request.Context())
	out.Close = true
	if request.Body != nil && request.Body != http.NoBody {
		x.upload = newReverseUpload(request.Body)
		out.Body = x.upload
	}
	go func() {
		defer close(x.writeDone)
		x.written <- out.Write(connection)
	}()
	go func() {
		defer close(x.readDone)
		response, err := readReverseResponse(connection, out)
		x.reply <- reverseHTTPReply{response, err}
	}()
	go func() {
		defer close(x.watchDone)
		if x.upload == nil {
			return
		}
		select {
		case <-x.upload.failure:
			x.connection.Close()
		case <-x.ended:
		}
	}()
	return x
}

func (x *reverseExchange) response(ctx context.Context) (*http.Response, error) {
	written := x.written
	for {
		select {
		case reply := <-x.reply:
			if x.upload != nil {
				if err := x.upload.err(); err != nil {
					return nil, err
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return reply.response, reply.err
		case err := <-written:
			written = nil
			if err != nil {
				// Close does not discard the stream's buffered response. Its HTTP
				// reader, not the failed writer, decides whether a final reply exists.
				x.connection.Close()
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (x *reverseExchange) close() {
	x.closeOnce.Do(func() {
		close(x.ended)
		if x.upload != nil {
			x.upload.finish()
			// Body.Close alone can wait on a read held by the client upload.
			// Interrupt the connection read first, only after response completion
			// or explicit cancellation, so an early final reply is not truncated.
			x.abort()
			x.upload.source.Close()
		}
		x.connection.Close()
		if x.upload != nil {
			<-x.upload.done
		}
		<-x.writeDone
		<-x.readDone
		<-x.watchDone
	})
}

const reverseHeaderLimit = 1 << 20

func readReverseResponse(connection io.Reader, request *http.Request) (*http.Response, error) {
	budget := &io.LimitedReader{R: connection, N: reverseHeaderLimit}
	reader := bufio.NewReader(budget)
	for count := 0; count < 9; count++ {
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return nil, fmt.Errorf("read reverse HTTP response: %w", err)
		}
		if response.StatusCode == http.StatusSwitchingProtocols {
			return nil, errors.New("reverse HTTP protocol switching is not supported")
		}
		if response.StatusCode >= 200 {
			// bufio may already hold body bytes. The same reader continues with
			// no body-size cap, without discarding or recounting those bytes.
			budget.N = math.MaxInt64
			return response, nil
		}
		response.Body.Close()
	}
	return nil, errors.New("too many reverse HTTP informational responses")
}
