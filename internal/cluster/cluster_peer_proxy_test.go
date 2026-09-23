package cluster

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// An application may answer a request before it has read all of the body,
// and the peer proxy flushes that answer to the console straight away. The
// rest of the body must still be forwarded and the answer relayed whole: a
// console that already holds a 200 must not be left with half of its JSON.
func TestPeerProxyRelaysWholeAnswerWhileRequestBodyIsStillForwarding(t *testing.T) {
	// Losing the answer depends on how the proxy's two readers of the body
	// interleave, so a handful of requests make a regression certain to show.
	for range 5 {
		relayEarlyAnswer(t)
	}
}

func relayEarlyAnswer(t *testing.T) {
	t.Helper()
	const head, size = 16, 64 << 10
	answered := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The application side answers early on purpose; it is the proxy in
		// between that is under test.
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		if _, err := io.ReadFull(r.Body, make([]byte, head)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"state":`)
		http.NewResponseController(w).Flush()
		close(answered)
		rest, _ := io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `"accepted","rest":`)
		_ = json.NewEncoder(w).Encode(rest)
		_, _ = io.WriteString(w, `}`)
	}))
	defer backend.Close()
	origin, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	peer := &Peer{}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.proxy(w, r, origin, "", "owner-token", 1, transport)
	}))
	defer front.Close()

	body, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, front.URL+"/console/services/hub/restart", body)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = size
	go func() {
		if _, err := writer.Write(bytes.Repeat([]byte("a"), head)); err != nil {
			return
		}
		// The application has started to answer, and the proxy is given a
		// moment to start relaying it; only then does the rest follow.
		<-answered
		time.Sleep(10 * time.Millisecond)
		_, err := writer.Write(bytes.Repeat([]byte("b"), size-head))
		_ = writer.CloseWithError(err)
	}()
	client := &http.Client{Transport: &http.Transport{}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	var decoded struct {
		State string `json:"state"`
		Rest  int64  `json:"rest"`
	}
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(answer, &decoded) != nil {
		t.Fatalf("answer = %d %q, %v", response.StatusCode, answer, err)
	}
	if decoded.State != "accepted" || decoded.Rest != size-head {
		t.Fatalf("answer = %+v, want the whole body forwarded", decoded)
	}
}
