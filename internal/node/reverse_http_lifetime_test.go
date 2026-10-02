package node

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReverseHTTPBodyCloseReleasesItsUploadAndStream(t *testing.T) {
	stop := make(chan struct{})
	finished := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		http.NewResponseController(w).EnableFullDuplex()
		drained := make(chan struct{})
		go func() { io.Copy(io.Discard, r.Body); close(drained) }()
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
			http.NewResponseController(w).SetReadDeadline(time.Now())
		}
		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Error("upstream reader was left running")
		}
	}))
	t.Cleanup(func() { close(stop); upstream.Close() })
	dial := reverseHTTPDialer(t, strings.TrimPrefix(upstream.URL, "http://"))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, reverseReadAbortKey{}, reverseReadAbort(reader.Close))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, reader)
	if err != nil {
		t.Fatal(err)
	}
	var connection reverseHTTPConnection
	transport := newReverseHTTP(func(ctx context.Context) (reverseHTTPConnection, error) {
		var err error
		connection, err = dial(ctx)
		return connection, err
	})
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("streaming response %q: %v", line, err)
	}
	body := response.Body.(*reverseResponseBody)
	closed := make(chan struct{})
	go func() {
		var closes sync.WaitGroup
		for range 3 {
			closes.Go(func() { response.Body.Close() })
		}
		cancel()
		closes.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Close and cancellation did not finish")
	}
	select {
	case <-body.upload.done:
	default:
		t.Fatal("response Close left its upload reader running")
	}
	select {
	case <-connection.Done():
	default:
		t.Fatal("response Close left its upstream stream open")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("body closure did not release upstream handler")
	}
}

func TestReverseHTTPWriteFailureWithoutAResponseFinishes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(server.Close)
	transport := newReverseHTTP(reverseHTTPDialer(t, strings.TrimPrefix(server.URL, "http://")))
	request := reverseRequest(t, "")
	request.Body = &failedReverseBody{}
	request.ContentLength = -1
	response, err := transport.RoundTrip(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("failed upload with no complete reply succeeded")
	}
}

type failedReverseBody struct{}

func (*failedReverseBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*failedReverseBody) Close() error             { return nil }

func TestReverseHTTPPreparationRequiresRealControls(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://node/mcp", nil)
	if err := prepareReverseHTTP(httptest.NewRecorder(), request); err == nil {
		t.Fatal("accepted request without a read interruption capability")
	}
	called := false
	ctx := context.WithValue(request.Context(), reverseReadAbortKey{}, reverseReadAbort(func() error { called = true; return nil }))
	if err := prepareReverseHTTP(httptest.NewRecorder(), request.WithContext(ctx)); !errors.Is(err, http.ErrNotSupported) || called {
		t.Fatalf("full duplex control missing: interrupted=%v error=%v", called, err)
	}
}

func TestReverseMCPEachClientConnectionCarriesOneRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	endpoint := reverseEndpoint(t, strings.TrimPrefix(server.URL, "http://"))
	connection, err := net.DialTimeout("tcp", strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(2 * time.Second))
	one := "POST /mcp HTTP/1.1\r\nHost: worker\r\nContent-Length: 0\r\n\r\n"
	io.WriteString(connection, one+one)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if _, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost}); err == nil {
		t.Fatal("a second request reused the interrupted client connection")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("pipeline invoked %d requests", got)
	}
}
