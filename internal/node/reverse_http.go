package node

import (
	"context"
	"net"
	"net/http"
)

type reverseHTTPConnection interface {
	net.Conn
	Done() <-chan struct{}
}

func newReverseHTTP(dial func(context.Context) (reverseHTTPConnection, error)) *http.Transport {
	return &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
	}
}
